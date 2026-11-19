// Added by the mouse9527/multica fork: password login (M-41) router wiring,
// and the admin reset to login end-to-end check (M-44).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/passwordauth"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestPasswordRoutesFollowTheSwitch(t *testing.T) {
	serve := func(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	configMentionsPasswords := func(h http.Handler) (any, bool) {
		var cfg map[string]any
		if err := json.Unmarshal(serve(h, http.MethodGet, "/api/config", "").Body.Bytes(), &cfg); err != nil {
			t.Fatal(err)
		}
		v, ok := cfg["password_login_enabled"]
		return v, ok
	}

	t.Setenv(passwordauth.EnvEnabled, "")
	off := NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	for _, route := range [][2]string{
		{http.MethodPost, "/auth/password-login"},
		{http.MethodGet, "/api/me/password"},
		{http.MethodPost, "/api/me/password"},
	} {
		if rec := serve(off, route[0], route[1], testToken); rec.Code != http.StatusNotFound {
			t.Fatalf("off: %s %s = %d, want 404", route[0], route[1], rec.Code)
		}
	}
	if _, ok := configMentionsPasswords(off); ok {
		t.Fatal("off: /api/config mentions password login")
	}

	t.Setenv(passwordauth.EnvEnabled, "true")
	on := NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	if rec := serve(on, http.MethodPost, "/auth/password-login", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("on: login with empty body = %d, want 400", rec.Code)
	}
	if rec := serve(on, http.MethodGet, "/api/me/password", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("on: password status without a session = %d, want 401", rec.Code)
	}
	if rec := serve(on, http.MethodGet, "/api/me/password", testToken); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"has_password":false`) {
		t.Fatalf("on: password status = %d %s", rec.Code, rec.Body)
	}
	if v, _ := configMentionsPasswords(on); v != true {
		t.Fatalf("on: password_login_enabled = %v", v)
	}
}

// A password set by the admin reset command signs in through
// POST /auth/password-login on the production router, also when the reset
// replaces the password of a locked account; the old password stops working.
// The reset runs the same store calls as reset_password after it has read
// the password, with the hashing settings from the environment, as in the
// container.
func TestAdminResetThenPasswordLogin(t *testing.T) {
	ctx := context.Background()
	// The cheapest parameters ParamsFromEnv accepts, for server and reset alike.
	t.Setenv(passwordauth.EnvMemoryKiB, "19456")
	t.Setenv(passwordauth.EnvIterations, "2")
	t.Setenv(passwordauth.EnvParallelism, "1")
	t.Setenv(passwordauth.EnvEnabled, "true")
	router := NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)

	email := fmt.Sprintf("pw-reset-%d@example.test", time.Now().UnixNano())
	userID := testutil.New(testPool, "", "").User(t, "Password Reset", email)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM user_credential WHERE user_id = $1`, userID)
	})

	hasher, err := passwordauth.HasherFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	store := passwordauth.NewStore(testPool, hasher, slog.New(slog.DiscardHandler))
	reset := func(password string) {
		t.Helper()
		id, err := store.UserIDByEmail(ctx, strings.ToUpper(email))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AdminReset(ctx, id, email, password, io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	const (
		first  = "first admin password"
		second = "second admin password"
		wrong  = "not the password at all"
	)
	var bodies strings.Builder
	addr := 0
	login := func(password string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"email": email, "password": password})
		req := httptest.NewRequest(http.MethodPost, "/auth/password-login", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		// A new client address per request keeps the per-IP limit out of it.
		addr++
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", addr)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		bodies.WriteString(rec.Body.String())
		bodies.WriteString(fmt.Sprint(rec.Header()))
		return rec
	}
	signIn := func(password string) {
		t.Helper()
		rec := login(password)
		var resp struct {
			Token string `json:"token"`
			User  struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || resp.Token == "" || resp.User.ID != userID {
			t.Fatalf("login after reset = %d %s", rec.Code, rec.Body)
		}
		// The session it returns is a working one.
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+resp.Token)
		me := httptest.NewRecorder()
		router.ServeHTTP(me, req)
		if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), userID) {
			t.Fatalf("GET /api/me with the new session = %d %s", me.Code, me.Body)
		}
	}

	// A first password, set by the administrator for a user who has none.
	reset(first)
	signIn(first)

	// Lock the account, then reset over the lock.
	for i := 0; i < passwordauth.LockThreshold; i++ {
		if rec := login(wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong guess %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := login(first); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked account = %d %s", rec.Code, rec.Body)
	}
	reset(second)
	signIn(second)
	if rec := login(first); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password after reset = %d %s", rec.Code, rec.Body)
	}

	for _, secret := range []string{first, second, wrong, "$argon2id$"} {
		if strings.Contains(bodies.String(), secret) {
			t.Fatalf("a response contains %q", secret)
		}
	}
}
