package githubissues

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2027, 1, 10, 9, 30, 0, 0, time.UTC)

// fakeGitHub serves GET /user. respond writes the response; the server fails
// the test if the token arrives anywhere but the Authorization header.
type fakeGitHub struct {
	server  *httptest.Server
	calls   atomic.Int32
	respond atomic.Value // func(http.ResponseWriter)
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/user" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fakeToken {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
		if strings.Contains(r.URL.String(), fakeToken[:12]) {
			t.Errorf("token leaked into the URL")
		}
		f.respond.Load().(func(http.ResponseWriter))(w)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) set(respond func(http.ResponseWriter)) { f.respond.Store(respond) }

func userOK(scopes ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if len(scopes) > 0 {
			w.Header().Set("X-OAuth-Scopes", scopes[0])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat","id":1}`))
	}
}

// failure echoes the token in its body, as a hostile or buggy upstream might,
// so tests prove response bodies never reach logs or errors.
func failure(status int, headers map[string]string, message string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"` + message + `","token":"` + fakeToken + `"}`))
	}
}

// syncBuffer collects log output written from the validation goroutine.
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

type testService struct {
	*Service
	logs *syncBuffer
	now  *time.Time
}

func newTestService(t *testing.T, apiBase, expiry string) testService {
	t.Helper()
	var logs syncBuffer
	now := testNow
	cfg := LoadConfig(envOf(map[string]string{
		EnvEnabled:        "true",
		EnvToken:          fakeToken,
		EnvTokenExpiresAt: expiry,
	}))
	svc := New(cfg, Options{
		APIBase: apiBase,
		Now:     func() time.Time { return now },
		Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if svc == nil {
		t.Fatalf("feature should be on, reason %q", cfg.DisabledReason())
	}
	t.Cleanup(func() { assertNoToken(t, "logs", logs.String()) })
	return testService{Service: svc, logs: &logs, now: &now}
}

func TestNewReturnsNilWhileOff(t *testing.T) {
	var logs bytes.Buffer
	svc := New(LoadConfig(envOf(nil)), Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if svc != nil {
		t.Fatal("New must return nil while the feature is off")
	}
	if !strings.Contains(logs.String(), "github issues disabled") {
		t.Fatalf("expected the disabled line, got %q", logs.String())
	}
}

func TestStatusBeforeFirstVerdictIsInvalid(t *testing.T) {
	svc := newTestService(t, "http://127.0.0.1:0", "2027-03-01")
	got := svc.TokenStatus()
	if got.Status != TokenInvalid || got.AccountLogin != nil || got.ValidatedAt != nil {
		t.Fatalf("status before validation = %+v", got)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != "2027-03-01" {
		t.Fatalf("expires_at = %v", got.ExpiresAt)
	}
	if svc.Usable() {
		t.Fatal("an unvalidated token must not be usable")
	}
}

func TestValidateAccepted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(http.ResponseWriter)
	}{
		{name: "repo scope listed", respond: userOK("repo, read:org")},
		{name: "repo scope among others", respond: userOK("gist,repo")},
		// Whether GET /user always sends the header is unverified; without it
		// the scope check is skipped.
		{name: "no scopes header", respond: userOK()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.set(tc.respond)
			svc := newTestService(t, gh.server.URL, "2027-03-01")

			if next := svc.validate(context.Background()); next != revalidateInterval {
				t.Fatalf("next check in %s, want %s", next, revalidateInterval)
			}
			got := svc.TokenStatus()
			if got.Status != TokenOK || !svc.Usable() {
				t.Fatalf("status = %+v, want ok", got)
			}
			if got.AccountLogin == nil || *got.AccountLogin != "octocat" {
				t.Fatalf("account_login = %v", got.AccountLogin)
			}
			if got.ValidatedAt == nil || !got.ValidatedAt.Equal(testNow) {
				t.Fatalf("validated_at = %v, want %v", got.ValidatedAt, testNow)
			}
		})
	}
}

func TestValidateRejected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		respond  func(http.ResponseWriter)
		category string
	}{
		{name: "401", respond: failure(http.StatusUnauthorized, nil, "Bad credentials"), category: rejectUnauthorized},
		{name: "403 not throttled", respond: failure(http.StatusForbidden, nil, "classic tokens are forbidden"), category: rejectForbidden},
		{
			name:     "403 sso",
			respond:  failure(http.StatusForbidden, map[string]string{"X-GitHub-SSO": "required; url=https://example.invalid/sso"}, "SSO required"),
			category: rejectSSORequired,
		},
		{name: "scopes without repo", respond: userOK("public_repo, gist"), category: rejectMissingRepoScope},
		{name: "empty scopes", respond: userOK(""), category: rejectMissingRepoScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			svc := newTestService(t, gh.server.URL, "never")

			// Accepted first, so the test also proves a rejection revokes it.
			gh.set(userOK("repo"))
			svc.validate(context.Background())
			if !svc.Usable() {
				t.Fatal("setup: token should be accepted")
			}

			*svc.now = testNow.Add(time.Hour)
			gh.set(tc.respond)
			if next := svc.validate(context.Background()); next != revalidateInterval {
				t.Fatalf("next check in %s, want the daily re-check", next)
			}
			got := svc.TokenStatus()
			if got.Status != TokenInvalid || svc.Usable() {
				t.Fatalf("status = %+v, want invalid", got)
			}
			if got.ValidatedAt == nil || !got.ValidatedAt.Equal(testNow.Add(time.Hour)) {
				t.Fatalf("validated_at = %v, want the rejection time", got.ValidatedAt)
			}
			if !strings.Contains(svc.logs.String(), `"category":"`+tc.category+`"`) {
				t.Fatalf("log should name category %s: %s", tc.category, svc.logs.String())
			}
		})
	}
}

// Response bodies stay out of errors: only the category or status code.
func TestFetchLoginErrorsCarryNoBody(t *testing.T) {
	for _, respond := range []func(http.ResponseWriter){
		failure(http.StatusUnauthorized, nil, "Bad credentials"),
		failure(http.StatusForbidden, nil, "Resource protected by organization SAML enforcement"),
		failure(http.StatusInternalServerError, nil, "boom"),
		failure(http.StatusNotFound, nil, "Not Found"),
	} {
		gh := newFakeGitHub(t)
		gh.set(respond)
		svc := newTestService(t, gh.server.URL, "never")
		_, err := svc.fetchLogin(context.Background())
		if err == nil {
			t.Fatal("expected an error")
		}
		assertNoToken(t, "error", err.Error())
		for _, word := range []string{"Bad credentials", "SAML", "boom", "Not Found"} {
			if strings.Contains(err.Error(), word) {
				t.Fatalf("error %q carries the response body", err)
			}
		}
	}
}

func TestValidateRateLimitedBacksOffAndKeepsVerdict(t *testing.T) {
	reset := strconv.FormatInt(testNow.Add(90*time.Second).Unix(), 10)
	for _, tc := range []struct {
		name    string
		respond func(http.ResponseWriter)
		wait    time.Duration
	}{
		{
			name:    "403 primary limit",
			respond: failure(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, "API rate limit exceeded"),
			wait:    90 * time.Second,
		},
		{
			name:    "403 secondary limit",
			respond: failure(http.StatusForbidden, map[string]string{"Retry-After": "42"}, "You have exceeded a secondary rate limit"),
			wait:    42 * time.Second,
		},
		{
			name:    "403 secondary limit by message",
			respond: failure(http.StatusForbidden, nil, "You have exceeded a secondary rate limit"),
			wait:    time.Minute,
		},
		{
			name:    "429",
			respond: failure(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, "slow down"),
			wait:    7 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			svc := newTestService(t, gh.server.URL, "never")

			// Before any verdict a rate limit leaves the token unvalidated.
			gh.set(tc.respond)
			if next := svc.validate(context.Background()); next != tc.wait {
				t.Fatalf("backoff = %s, want %s", next, tc.wait)
			}
			if got := svc.TokenStatus(); got.Status != TokenInvalid || got.ValidatedAt != nil {
				t.Fatalf("status = %+v, want unvalidated", got)
			}

			// After acceptance a rate limit is not a verdict on the token.
			gh.set(userOK("repo"))
			svc.validate(context.Background())
			gh.set(tc.respond)
			if next := svc.validate(context.Background()); next != tc.wait {
				t.Fatalf("backoff = %s, want %s", next, tc.wait)
			}
			if !svc.Usable() {
				t.Fatal("a rate limit must not invalidate an accepted token")
			}
		})
	}
}

func TestValidateTransientFailureRetriesWithBackoff(t *testing.T) {
	gh := newFakeGitHub(t)
	svc := newTestService(t, gh.server.URL, "never")
	gh.set(userOK("repo"))
	svc.validate(context.Background())

	gh.set(failure(http.StatusBadGateway, nil, "bad gateway"))
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := svc.validate(context.Background()); got != w {
			t.Fatalf("attempt %d: backoff = %s, want %s", i+1, got, w)
		}
	}
	if !svc.Usable() {
		t.Fatal("a 5xx says nothing about the token; the last verdict stands")
	}

	gh.set(userOK("repo"))
	if got := svc.validate(context.Background()); got != revalidateInterval {
		t.Fatalf("after recovery next check in %s", got)
	}
	gh.set(failure(http.StatusBadGateway, nil, "bad gateway"))
	if got := svc.validate(context.Background()); got != retryBaseDelay {
		t.Fatalf("backoff after recovery = %s, want it reset to %s", got, retryBaseDelay)
	}
}

func TestValidateNetworkErrorIsTransient(t *testing.T) {
	gh := newFakeGitHub(t)
	url := gh.server.URL
	gh.server.Close()
	svc := newTestService(t, url, "never")
	if got := svc.validate(context.Background()); got != retryBaseDelay {
		t.Fatalf("backoff = %s, want %s", got, retryBaseDelay)
	}
	if got := svc.TokenStatus(); got.Status != TokenInvalid || got.ValidatedAt != nil {
		t.Fatalf("status = %+v, want unvalidated", got)
	}
}

func TestExpiredTokenIsNotSentAndStopsTheFeature(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.set(userOK("repo"))
	svc := newTestService(t, gh.server.URL, "2027-02-01")
	svc.validate(context.Background())
	if !svc.Usable() {
		t.Fatal("setup: token should be accepted")
	}
	calls := gh.calls.Load()

	// The expiry date itself already counts as expired.
	*svc.now = time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	if got := svc.TokenStatus(); got.Status != TokenExpired || svc.Usable() {
		t.Fatalf("status on the expiry date = %+v, want expired", got)
	}
	if next := svc.validate(context.Background()); next != revalidateInterval {
		t.Fatalf("next check in %s", next)
	}
	if gh.calls.Load() != calls {
		t.Fatal("an expired token must not be sent to GitHub")
	}
	*svc.now = time.Date(2027, 1, 31, 23, 59, 59, 0, time.UTC)
	if !svc.Usable() {
		t.Fatal("the day before the expiry date the token is still usable")
	}
}

func TestStartValidatesInBackgroundAndStops(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.set(userOK("repo"))
	svc := newTestService(t, gh.server.URL, "never")
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !svc.Usable() {
		if time.Now().After(deadline) {
			t.Fatal("Start did not validate the token")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

func TestTokenStatusJSONHasOnlyPublicFields(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.set(userOK("repo"))
	for _, expiry := range []string{"2027-03-01", "never"} {
		svc := newTestService(t, gh.server.URL, expiry)
		for _, step := range []string{"unvalidated", "accepted"} {
			if step == "accepted" {
				svc.validate(context.Background())
			}
			encoded, err := json.Marshal(svc.TokenStatus())
			if err != nil {
				t.Fatal(err)
			}
			assertNoToken(t, "status json", string(encoded))
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 4 {
				t.Fatalf("%s/%s: fields = %v, want exactly 4", expiry, step, fields)
			}
			for _, key := range []string{"account_login", "status", "expires_at", "validated_at"} {
				if _, ok := fields[key]; !ok {
					t.Fatalf("%s/%s: missing %s in %s", expiry, step, key, encoded)
				}
			}
			if expiry == "never" && fields["expires_at"] != nil {
				t.Fatalf("never-expiring token: expires_at = %v, want null", fields["expires_at"])
			}
		}
	}
}

func TestHandleTokenStatus(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.set(userOK("repo"))
	svc := newTestService(t, gh.server.URL, "2027-03-01")
	svc.validate(context.Background())

	rec := httptest.NewRecorder()
	svc.HandleTokenStatus(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	assertNoToken(t, "response", rec.Body.String())
	var got TokenStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != TokenOK || got.AccountLogin == nil || *got.AccountLogin != "octocat" {
		t.Fatalf("response = %+v", got)
	}
}
