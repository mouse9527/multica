// Added by the mouse9527/multica fork: password login (M-41) router wiring.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/passwordauth"
	"github.com/multica-ai/multica/server/internal/realtime"
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
