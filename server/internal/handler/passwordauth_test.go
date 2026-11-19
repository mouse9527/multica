package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/passwordauth"
)

const (
	pwTestPassword  = "correct horse battery"
	pwWrongPassword = "wrong password, twelve+"
)

func passthrough(next http.Handler) http.Handler { return next }

// passwordTestServer mounts the password routes on a fresh router with cheap
// hashing. Requests reach /api/me/password with whatever X-User-ID the test
// sets, as they would after the session middleware.
type passwordTestServer struct {
	t      *testing.T
	router chi.Router
	svc    *passwordauth.Service
	logs   *bytes.Buffer
	ip     atomic.Int32
}

func newPasswordTestServer(t *testing.T) *passwordTestServer {
	t.Helper()
	hasher, err := passwordauth.NewHasher(passwordauth.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(logger)
	svc := passwordauth.NewService(passwordauth.NewStore(testPool, hasher, logger), logger)
	router := chi.NewRouter()
	testHandler.mountPasswordAuth(router, svc, passthrough, nil)
	t.Cleanup(func() {
		slog.SetDefault(prev)
		testHandler.PasswordAuth = nil
	})
	return &passwordTestServer{t: t, router: router, svc: svc, logs: logs}
}

// do sends a request from a fresh client address, so the per-IP limit stays
// out of tests that are not about it.
func (s *passwordTestServer) do(method, path, userID string, body any, header ...string) *httptest.ResponseRecorder {
	s.t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			s.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	n := s.ip.Add(1)
	req.RemoteAddr = fmt.Sprintf("198.51.%d.%d:1234", n/250, n%250+1)
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func (s *passwordTestServer) login(email, password string) *httptest.ResponseRecorder {
	return s.do(http.MethodPost, "/auth/password-login", "", PasswordLoginRequest{Email: email, Password: password})
}

// assertNoSecrets checks text for the passwords and for every stored hash.
func (s *passwordTestServer) assertNoSecrets(text string, passwords ...string) {
	s.t.Helper()
	for _, p := range passwords {
		if strings.Contains(text, p) {
			s.t.Fatalf("output contains a password: %s", text)
		}
	}
	rows, err := testPool.Query(context.Background(), `SELECT password_hash FROM user_credential`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			s.t.Fatal(err)
		}
		if strings.Contains(text, hash) || strings.Contains(text, hash[strings.LastIndex(hash, "$")+1:]) {
			s.t.Fatal("output contains a stored hash")
		}
	}
}

func pwTestUser(t *testing.T, label string) (id, email string) {
	t.Helper()
	email = fmt.Sprintf("pw-%s-%d@example.test", label, time.Now().UnixNano())
	id = dbfx.User(t, "password "+label, email)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM user_credential WHERE user_id = $1`, id)
	})
	return id, email
}

func getConfigJSON(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	testHandler.GetConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPasswordAuthSwitch(t *testing.T) {
	t.Cleanup(func() { testHandler.PasswordAuth = nil })
	routes := func() chi.Router {
		r := chi.NewRouter()
		testHandler.MountPasswordAuth(r, passthrough, nil)
		return r
	}
	status := func(r chi.Router, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("X-User-ID", testUserID)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, value := range []string{"", "false"} {
		t.Setenv(passwordauth.EnvEnabled, value)
		r := routes()
		for _, route := range [][2]string{
			{http.MethodPost, "/auth/password-login"},
			{http.MethodGet, "/api/me/password"},
			{http.MethodPost, "/api/me/password"},
		} {
			if got := status(r, route[0], route[1]); got != http.StatusNotFound {
				t.Fatalf("switch %q: %s %s = %d, want 404", value, route[0], route[1], got)
			}
		}
		if _, ok := getConfigJSON(t)["password_login_enabled"]; ok {
			t.Fatalf("switch %q: /api/config mentions password login", value)
		}
	}

	// Invalid argon2id settings keep the feature off.
	t.Setenv(passwordauth.EnvEnabled, "true")
	t.Setenv(passwordauth.EnvMemoryKiB, "1")
	if got := status(routes(), http.MethodPost, "/auth/password-login"); got != http.StatusNotFound {
		t.Fatalf("invalid settings: login = %d, want 404", got)
	}
	if _, ok := getConfigJSON(t)["password_login_enabled"]; ok {
		t.Fatal("invalid settings: /api/config mentions password login")
	}

	t.Setenv(passwordauth.EnvMemoryKiB, "")
	r := routes()
	if got := status(r, http.MethodPost, "/auth/password-login"); got != http.StatusBadRequest {
		t.Fatalf("on: login with empty body = %d, want 400", got)
	}
	if got := status(r, http.MethodGet, "/api/me/password"); got != http.StatusOK {
		t.Fatalf("on: password status = %d, want 200", got)
	}
	if v := getConfigJSON(t)["password_login_enabled"]; v != true {
		t.Fatalf("on: password_login_enabled = %v", v)
	}
}

func TestPasswordLogin(t *testing.T) {
	s := newPasswordTestServer(t)
	ctx := context.Background()
	userID, email := pwTestUser(t, "login")
	_, noPasswordEmail := pwTestUser(t, "nopw")
	if err := s.svc.SetPassword(ctx, parseUUID(userID), "", pwTestPassword); err != nil {
		t.Fatal(err)
	}

	rec := s.login(strings.ToUpper(email), pwTestPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	var resp LoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" || resp.User.ID != userID {
		t.Fatalf("login response %s: %v", rec.Body, err)
	}
	var authCookie bool
	for _, c := range rec.Result().Cookies() {
		authCookie = authCookie || (c.Name == auth.AuthCookieName && c.Value == resp.Token)
	}
	if !authCookie {
		t.Fatal("login did not set the auth cookie")
	}
	s.assertNoSecrets(rec.Body.String()+fmt.Sprint(rec.Header()), pwTestPassword)

	// Wrong password, user without a password, unknown email: one answer.
	wrong := s.login(email, pwWrongPassword)
	for name, got := range map[string]*httptest.ResponseRecorder{
		"no password":   s.login(noPasswordEmail, pwWrongPassword),
		"unknown email": s.login("pw-nobody@example.test", pwWrongPassword),
	} {
		if got.Code != wrong.Code || got.Body.String() != wrong.Body.String() {
			t.Fatalf("%s: %d %s; wrong password: %d %s", name, got.Code, got.Body, wrong.Code, wrong.Body)
		}
	}
	if wrong.Code != http.StatusUnauthorized || !strings.Contains(wrong.Body.String(), passwordCodeInvalidCredentials) {
		t.Fatalf("wrong password: %d %s", wrong.Code, wrong.Body)
	}

	if rec := s.do(http.MethodPost, "/auth/password-login", "", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", rec.Code)
	}
	s.assertNoSecrets(s.logs.String(), pwTestPassword, pwWrongPassword)
}

func TestPasswordLoginLockout(t *testing.T) {
	s := newPasswordTestServer(t)
	userID, email := pwTestUser(t, "lock")
	if err := s.svc.SetPassword(context.Background(), parseUUID(userID), "", pwTestPassword); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < passwordauth.LockThreshold; i++ {
		if rec := s.login(email, pwWrongPassword); rec.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d", i+1, rec.Code)
		}
		s.login("pw-nobody-lock@example.test", pwWrongPassword)
	}
	locked := s.login(email, pwTestPassword)
	if locked.Code != http.StatusTooManyRequests || !strings.Contains(locked.Body.String(), passwordCodeThrottled) {
		t.Fatalf("locked account: %d %s", locked.Code, locked.Body)
	}
	phantom := s.login("pw-nobody-lock@example.test", pwWrongPassword)
	if phantom.Code != locked.Code || phantom.Body.String() != locked.Body.String() {
		t.Fatalf("unknown email after the same guesses: %d %s", phantom.Code, phantom.Body)
	}

	// The per-IP limit answers the same way.
	var rec *httptest.ResponseRecorder
	for i := 0; i <= passwordauth.IPLimit; i++ {
		req := httptest.NewRequest(http.MethodPost, "/auth/password-login",
			strings.NewReader(fmt.Sprintf(`{"email":"pw-spray-%d@example.test","password":"x"}`, i)))
		req.RemoteAddr = "203.0.113.50:999"
		rec = httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
	}
	if rec.Code != locked.Code || rec.Body.String() != locked.Body.String() {
		t.Fatalf("IP limit: %d %s", rec.Code, rec.Body)
	}
	s.assertNoSecrets(s.logs.String(), pwTestPassword, pwWrongPassword)
}

func TestSetMyPassword(t *testing.T) {
	s := newPasswordTestServer(t)
	userID, email := pwTestUser(t, "set")
	const second = "second password, long"
	call := func(current, next string, header ...string) *httptest.ResponseRecorder {
		return s.do(http.MethodPost, "/api/me/password", userID, SetPasswordRequest{CurrentPassword: current, NewPassword: next}, header...)
	}
	status := func() bool {
		rec := s.do(http.MethodGet, "/api/me/password", userID, nil)
		var out PasswordStatusResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("status: %d %s", rec.Code, rec.Body)
		}
		return out.HasPassword
	}

	if status() {
		t.Fatal("has_password before setting one")
	}
	// Only a person's own session may manage the password.
	if rec := call("", pwTestPassword, "Authorization", "Bearer mul_test"); rec.Code != http.StatusForbidden {
		t.Fatalf("PAT = %d", rec.Code)
	}
	if rec := call("", pwTestPassword, "X-Actor-Source", "task_token"); rec.Code != http.StatusForbidden {
		t.Fatalf("task token = %d", rec.Code)
	}
	if rec := call("", "too short"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), passwordCodeInvalidPassword) {
		t.Fatalf("short password: %d %s", rec.Code, rec.Body)
	}
	if status() {
		t.Fatal("refused password was stored")
	}

	if rec := call("", pwTestPassword); rec.Code != http.StatusOK {
		t.Fatalf("first password: %d %s", rec.Code, rec.Body)
	}
	if !status() {
		t.Fatal("has_password false after setting one")
	}
	if rec := call("", second); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), passwordCodeCurrentRequired) {
		t.Fatalf("change without current: %d %s", rec.Code, rec.Body)
	}
	if rec := call(pwWrongPassword, second); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), passwordCodeCurrentIncorrect) {
		t.Fatalf("wrong current: %d %s", rec.Code, rec.Body)
	}
	if rec := call(pwTestPassword, second); rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	if rec := s.login(email, second); rec.Code != http.StatusOK {
		t.Fatalf("login with new password: %d %s", rec.Code, rec.Body)
	}
	s.assertNoSecrets(s.logs.String(), pwTestPassword, pwWrongPassword, second)
}

// The endpoint only acts on the session's existing user: it cannot create a
// user or attach a password to an email nobody has.
func TestSetMyPasswordNeedsExistingUser(t *testing.T) {
	s := newPasswordTestServer(t)
	ghost := "00000000-0000-4000-8000-00000000abcd"
	rec := s.do(http.MethodPost, "/api/me/password", ghost,
		map[string]string{"new_password": pwTestPassword, "email": "pw-ghost@example.test"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user = %d %s", rec.Code, rec.Body)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM user_credential WHERE user_id = $1) + (SELECT count(*) FROM "user" WHERE email = 'pw-ghost@example.test')`,
		ghost).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows created: %d %v", n, err)
	}
	if rec := s.login("pw-ghost@example.test", pwTestPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login as ghost = %d", rec.Code)
	}
}
