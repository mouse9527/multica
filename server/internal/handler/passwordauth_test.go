package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/passwordauth"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	pwTestPassword  = "correct horse battery"
	pwWrongPassword = "wrong password, twelve+"
)

// realAuth is the production session middleware, so the tests see the
// credential kinds exactly as the server does.
func realAuth() func(http.Handler) http.Handler {
	return middleware.Auth(testHandler.Queries, nil, nil, nil)
}

// pwSession returns a login session JWT for an existing user.
func pwSession(t *testing.T, userID string) string {
	t.Helper()
	user, err := testHandler.Queries.GetUser(context.Background(), parseUUID(userID))
	if err != nil {
		t.Fatal(err)
	}
	token, err := testHandler.issueJWT(user)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// tokenBoundCSRF builds the CSRF value ValidateCSRF accepts for a cookie
// holding token, the way any holder of the token can.
func tokenBoundCSRF(t *testing.T, token string) string {
	t.Helper()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(nonce)
	return hex.EncodeToString(nonce) + "." + hex.EncodeToString(mac.Sum(nil))
}

// passwordTestServer mounts the password routes on a fresh router with cheap
// hashing behind the real session middleware.
type passwordTestServer struct {
	t      *testing.T
	router chi.Router
	svc    *passwordauth.Service
	logs   *syncBuffer
	ip     atomic.Int32
}

// syncBuffer is a bytes.Buffer safe for concurrent use. The password tests
// make it the default logger's output, and goroutines other tests in this
// package leave running log through the default logger too.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pwCheapParams keeps hashing fast in tests.
var pwCheapParams = passwordauth.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}

func newPasswordTestServer(t *testing.T) *passwordTestServer {
	t.Helper()
	hasher, err := passwordauth.NewHasher(pwCheapParams)
	if err != nil {
		t.Fatal(err)
	}
	return newPasswordTestServerWith(t, hasher)
}

func newPasswordTestServerWith(t *testing.T, hasher *passwordauth.Hasher) *passwordTestServer {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(logger)
	svc := passwordauth.NewService(passwordauth.NewStore(testPool, hasher, logger), logger)
	router := chi.NewRouter()
	testHandler.mountPasswordAuth(router, svc, realAuth(), nil)
	t.Cleanup(func() {
		slog.SetDefault(prev)
		testHandler.PasswordAuth = nil
	})
	return &passwordTestServer{t: t, router: router, svc: svc, logs: logs}
}

// do sends a request from a fresh client address, so the per-IP limit stays
// out of tests that are not about it. A non-empty token is sent as a bearer
// token.
func (s *passwordTestServer) do(method, path, token string, body any, header ...string) *httptest.ResponseRecorder {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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
		testHandler.MountPasswordAuth(r, realAuth(), nil)
		return r
	}
	session := pwSession(t, testUserID)
	status := func(r chi.Router, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+session)
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
		for _, field := range []string{"password_login_enabled", "password_min_length"} {
			if _, ok := getConfigJSON(t)[field]; ok {
				t.Fatalf("switch %q: /api/config has %s", value, field)
			}
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
	if v := getConfigJSON(t)["password_min_length"]; v != float64(passwordauth.DefaultMinPasswordRunes) {
		t.Fatalf("on: password_min_length = %v, want the default", v)
	}
}

// MULTICA_PASSWORD_MIN_LENGTH reaches /api/config and the set-password
// route; a value below the floor is raised to it.
func TestPasswordMinLengthFromEnv(t *testing.T) {
	t.Cleanup(func() { testHandler.PasswordAuth = nil })
	t.Setenv(passwordauth.EnvEnabled, "true")
	// The cheapest parameters ParamsFromEnv accepts.
	t.Setenv(passwordauth.EnvMemoryKiB, "19456")
	t.Setenv(passwordauth.EnvIterations, "2")
	t.Setenv(passwordauth.EnvParallelism, "1")

	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "8", want: 8},
		{value: "3", want: passwordauth.MinPasswordRunesFloor},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(passwordauth.EnvMinLength, tc.value)
			r := chi.NewRouter()
			testHandler.MountPasswordAuth(r, realAuth(), nil)
			if v := getConfigJSON(t)["password_min_length"]; v != float64(tc.want) {
				t.Fatalf("password_min_length = %v, want %d", v, tc.want)
			}

			userID, _ := pwTestUser(t, "minlen")
			session := pwSession(t, userID)
			set := func(password string) *httptest.ResponseRecorder {
				body, _ := json.Marshal(SetPasswordRequest{NewPassword: password})
				req := httptest.NewRequest(http.MethodPost, "/api/me/password", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+session)
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				return rec
			}
			rec := set(strings.Repeat("a", tc.want-1))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), passwordCodeInvalidPassword) ||
				!strings.Contains(rec.Body.String(), fmt.Sprintf("at least %d characters", tc.want)) {
				t.Fatalf("%d characters: %d %s", tc.want-1, rec.Code, rec.Body)
			}
			if rec := set(strings.Repeat("a", tc.want)); rec.Code != http.StatusOK {
				t.Fatalf("%d characters: %d %s", tc.want, rec.Code, rec.Body)
			}
		})
	}
}

// MULTICA_PASSWORD_IP_LIMIT reaches the service MountPasswordAuth builds:
// unset, one address gets 429 after IPLimit attempts; "0", it keeps getting
// 401 and the startup log says the limit is off.
func TestPasswordIPLimitSwitch(t *testing.T) {
	t.Cleanup(func() { testHandler.PasswordAuth = nil })
	t.Setenv(passwordauth.EnvEnabled, "true")
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	lastOfSpray := func(value string) int {
		t.Setenv(passwordauth.EnvIPLimit, value)
		r := chi.NewRouter()
		testHandler.MountPasswordAuth(r, realAuth(), nil)
		var rec *httptest.ResponseRecorder
		for i := 0; i <= passwordauth.IPLimit; i++ {
			req := httptest.NewRequest(http.MethodPost, "/auth/password-login",
				strings.NewReader(fmt.Sprintf(`{"email":"pw-ipswitch-%s-%d@example.test","password":"x"}`, value, i)))
			req.RemoteAddr = "203.0.113.60:999"
			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, req)
		}
		return rec.Code
	}

	if got := lastOfSpray(""); got != http.StatusTooManyRequests {
		t.Fatalf("unset: attempt %d from one address = %d, want 429", passwordauth.IPLimit+1, got)
	}
	if strings.Contains(logs.String(), "per-IP limit off") {
		t.Fatalf("unset: logged the limit as off: %s", logs)
	}
	if got := lastOfSpray("0"); got != http.StatusUnauthorized {
		t.Fatalf("off: attempt %d from one address = %d, want 401", passwordauth.IPLimit+1, got)
	}
	if !strings.Contains(logs.String(), "per-IP limit off") {
		t.Fatalf("off: startup did not log the limit as off: %s", logs)
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
	if !strings.Contains(s.logs.String(), "user logged in with password") || !strings.Contains(s.logs.String(), "ip=198.51.") {
		t.Fatalf("successful login not logged with its address: %s", s.logs)
	}

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
	session := pwSession(t, userID)
	const second = "second password, long"
	call := func(current, next string) *httptest.ResponseRecorder {
		return s.do(http.MethodPost, "/api/me/password", session, SetPasswordRequest{CurrentPassword: current, NewPassword: next})
	}
	status := func() bool {
		rec := s.do(http.MethodGet, "/api/me/password", session, nil)
		var out PasswordStatusResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("status: %d %s", rec.Code, rec.Body)
		}
		return out.HasPassword
	}

	if status() {
		t.Fatal("has_password before setting one")
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
	session, err := testHandler.issueJWT(db.User{ID: parseUUID(ghost), Email: "pw-ghost@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	rec := s.do(http.MethodPost, "/api/me/password", session,
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

// Password management takes the credential Auth actually used: a personal
// access token is refused as a bearer token and inside the auth cookie, even
// with the CSRF value its holder can compute, while a session JWT in the same
// cookie with the same kind of CSRF value is accepted.
func TestPasswordManagementRefusesPATs(t *testing.T) {
	s := newPasswordTestServer(t)
	userID, _ := pwTestUser(t, "pat")
	pat := "mul_" + hex.EncodeToString([]byte(fmt.Sprintf("pw-pat-%d", time.Now().UnixNano())))
	dbfx.Insert(t, "personal_access_token", testutil.Cols{
		"user_id": userID, "name": "password test", "token_hash": auth.HashToken(pat), "token_prefix": pat[:12],
	})
	body := SetPasswordRequest{NewPassword: pwTestPassword}
	cookie := func(token string) []string {
		return []string{"Cookie", auth.AuthCookieName + "=" + token, auth.CSRFHeaderName, tokenBoundCSRF(t, token)}
	}

	// The PAT itself authenticates (so a 403 below is this guard, not Auth).
	if rec := s.do(http.MethodPost, "/api/me/password", pat, body); rec.Code != http.StatusForbidden {
		t.Fatalf("PAT bearer = %d %s", rec.Code, rec.Body)
	}
	if rec := s.do(http.MethodPost, "/api/me/password", "", body, cookie(pat)...); rec.Code != http.StatusForbidden {
		t.Fatalf("PAT cookie = %d %s", rec.Code, rec.Body)
	}
	if rec := s.do(http.MethodGet, "/api/me/password", "", nil, cookie(pat)...); rec.Code != http.StatusForbidden {
		t.Fatalf("PAT cookie status = %d %s", rec.Code, rec.Body)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM user_credential WHERE user_id = $1`, userID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("PAT set a password: %d %v", n, err)
	}

	if rec := s.do(http.MethodPost, "/api/me/password", "", body, cookie(pwSession(t, userID))...); rec.Code != http.StatusOK {
		t.Fatalf("session cookie = %d %s", rec.Code, rec.Body)
	}
}

// When the stored hash needs more memory than the hashing budget allows, the
// settings page gets a translatable 409 instead of a generic 500.
func TestSetMyPasswordHashOverBudget(t *testing.T) {
	userID, _ := pwTestUser(t, "budget")
	old := pwCheapParams
	old.MemoryKiB = 128
	oldHasher, err := passwordauth.NewHasher(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := passwordauth.NewStore(testPool, oldHasher, nil).SetPassword(context.Background(), parseUUID(userID), pwTestPassword); err != nil {
		t.Fatal(err)
	}
	small, err := passwordauth.NewHasherWithBudget(pwCheapParams, pwCheapParams.MemoryKiB)
	if err != nil {
		t.Fatal(err)
	}
	s := newPasswordTestServerWith(t, small)
	rec := s.do(http.MethodPost, "/api/me/password", pwSession(t, userID),
		SetPasswordRequest{CurrentPassword: pwTestPassword, NewPassword: "second password, long"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), passwordCodeResetRequired) {
		t.Fatalf("over-budget hash: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(s.logs.String(), passwordauth.EnvMemoryBudgetKiB) {
		t.Fatalf("operator not told: %s", s.logs)
	}
	s.assertNoSecrets(rec.Body.String()+s.logs.String(), pwTestPassword)
}
