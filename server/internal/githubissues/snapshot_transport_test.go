package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

const snapshotHead = "0123abcd0123abcd0123abcd0123abcd0123abcd"

// toServer sends every request to the fake GitHub, whatever host it names,
// so a request the transport wrongly let through is seen by the server.
type toServer struct{ target *url.URL }

func (s toServer) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = s.target.Scheme, s.target.Host, ""
	return http.DefaultTransport.RoundTrip(out)
}

// fakeSnapshotAPI serves the reads of FetchPRSnapshot for acme/widgets#7 and
// records every request. The token must arrive in the Authorization header.
type fakeSnapshotAPI struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
	respond  func(w http.ResponseWriter, r *http.Request) bool
}

func (f *fakeSnapshotAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	respond := f.respond
	f.mu.Unlock()
	assertNoToken(f.t, "request URL", r.URL.String())
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		f.t.Errorf("request without the token: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if respond != nil && respond(w, r) {
		return
	}
	switch r.URL.Path {
	case "/repos/acme/widgets/pulls/7":
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]any{"sha": snapshotHead}, "mergeable": true, "mergeable_state": "clean"})
	case "/repos/acme/widgets/commits/" + snapshotHead + "/check-runs":
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{"id": 1, "head_sha": snapshotHead, "name": "build", "status": "completed", "conclusion": "success"},
			{"id": 2, "head_sha": snapshotHead, "name": "test", "status": "in_progress"},
		}})
	case "/repos/acme/widgets/commits/" + snapshotHead + "/status":
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "statuses": []any{}})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeSnapshotAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

type snapshotFixture struct {
	testService
	api    *fakeSnapshotAPI
	store  *fakeRepoStore
	client *ghsnapshot.Client
}

// newSnapshotFixture wires the production pieces: a Service with the default
// API root, its SnapshotTransport as the static source of a ghsnapshot client,
// and only the network redirected to a fake GitHub.
func newSnapshotFixture(t *testing.T) snapshotFixture {
	t.Helper()
	api := &fakeSnapshotAPI{t: t}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	target, _ := url.Parse(api.server.URL)

	svc := newTestService(t, "", "2027-03-01")
	svc.repoHTTPClient = noRedirectClient(&http.Client{Transport: toServer{target: target}, Timeout: 5 * time.Second})
	store := newFakeRepoStore()
	store.register(testUUID(1), "https://github.com/acme/widgets.git")
	svc.store = store
	svc.state.markAccepted("octocat", testNow)
	return snapshotFixture{
		testService: svc, api: api, store: store,
		client: (*ghsnapshot.Client)(nil).WithStaticSource(svc.SnapshotTransport()),
	}
}

func (f snapshotFixture) fetch(t *testing.T) (*ghsnapshot.PRSnapshot, error) {
	t.Helper()
	snap, err := ghsnapshot.FetchPRSnapshot(context.Background(), f.client, ghsnapshot.StaticInstallationID, "acme", "widgets", 7)
	if err != nil {
		assertNoToken(t, "error", err.Error())
	}
	return snap, err
}

func TestSnapshotTransportFetchesPRSnapshot(t *testing.T) {
	f := newSnapshotFixture(t)
	snap, err := f.fetch(t)
	if err != nil {
		t.Fatal(err)
	}
	if snap.HeadSHA != snapshotHead || snap.Mergeable != "MERGEABLE" || snap.MergeStateStatus != "CLEAN" ||
		!snap.HasChecks || snap.RollupState != "PENDING" || len(snap.Contexts) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if c := snap.Contexts[0]; c.Name != "build" || c.Status != "completed" || c.Conclusion != "success" {
		t.Fatalf("check run = %+v", c)
	}
	want := []string{
		"GET /repos/acme/widgets/pulls/7",
		"GET /repos/acme/widgets/commits/" + snapshotHead + "/check-runs",
		"GET /repos/acme/widgets/commits/" + snapshotHead + "/status",
		"GET /repos/acme/widgets/pulls/7",
	}
	if got := f.api.seen(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestSnapshotTransportAllowlistIsServerWide(t *testing.T) {
	f := newSnapshotFixture(t)
	f.store.unregisterAll(testUUID(1))
	f.store.register(testUUID(2), "https://github.com/Acme/Widgets")
	if _, err := f.fetch(t); err != nil {
		t.Fatalf("a repository registered in another workspace must be readable: %v", err)
	}
}

func TestSnapshotTransportRefusesWithoutSending(t *testing.T) {
	f := newSnapshotFixture(t)
	transport := f.SnapshotTransport()
	pr := "https://api.github.com/repos/acme/widgets/pulls/7"
	cases := []struct {
		name, method, url string
		setup             func()
		want              error
	}{
		{name: "write", method: http.MethodPost, url: pr, want: errNotSnapshotRead},
		{name: "other host", method: http.MethodGet, url: "https://evil.example.com/repos/acme/widgets/pulls/7", want: errNotSnapshotRead},
		{name: "plain http", method: http.MethodGet, url: "http://api.github.com/repos/acme/widgets/pulls/7", want: errNotSnapshotRead},
		{name: "user info", method: http.MethodGet, url: "https://x@api.github.com/repos/acme/widgets/pulls/7", want: errNotSnapshotRead},
		{name: "issues", method: http.MethodGet, url: "https://api.github.com/repos/acme/widgets/issues/7", want: errNotSnapshotRead},
		{name: "contents", method: http.MethodGet, url: "https://api.github.com/repos/acme/widgets/contents/README.md", want: errNotSnapshotRead},
		{name: "by id", method: http.MethodGet, url: "https://api.github.com/repositories/42/pulls/7", want: errNotSnapshotRead},
		{name: "dot segments", method: http.MethodGet, url: "https://api.github.com/repos/acme/widgets/../secret/pulls/7", want: errNotSnapshotRead},
		{name: "encoded slash", method: http.MethodGet, url: "https://api.github.com/repos/acme%2Fsecret/widgets/pulls/7", want: errNotSnapshotRead},
		{name: "commit ref not a sha", method: http.MethodGet, url: "https://api.github.com/repos/acme/widgets/commits/main/status", want: errNotSnapshotRead},
		{name: "unregistered", method: http.MethodGet, url: "https://api.github.com/repos/acme/secret/pulls/7", want: ErrRepoNotRegistered},
		{name: "token rejected", method: http.MethodGet, url: pr, setup: func() { f.state.markRejected(testNow) }, want: ErrTokenNotUsable},
		{name: "token expired", method: http.MethodGet, url: pr, setup: func() {
			f.state.markAccepted("octocat", testNow)
			*f.now = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
		}, want: ErrTokenNotUsable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if req.Header.Get("Authorization") != "" {
				t.Fatal("the caller's request must not be given the token")
			}
		})
	}
	if got := f.api.seen(); len(got) != 0 {
		t.Fatalf("refused requests reached GitHub: %v", got)
	}
}

func TestSnapshotTransportChecksRedirectTargets(t *testing.T) {
	for _, location := range []string{
		"https://api.github.com/repositories/42/pulls/7",
		"https://evil.example.com/repos/acme/widgets/pulls/7",
	} {
		t.Run(location, func(t *testing.T) {
			f := newSnapshotFixture(t)
			f.api.respond = func(w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusMovedPermanently)
				return true
			}
			if _, err := f.fetch(t); err == nil {
				t.Fatal("a redirect off the snapshot reads must fail")
			}
			if got := f.api.seen(); len(got) != 1 {
				t.Fatalf("the redirect was followed with the token: %v", got)
			}
		})
	}
}

func TestSnapshotTransportUnauthorizedStopsToken(t *testing.T) {
	f := newSnapshotFixture(t)
	f.api.respond = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return true
	}
	if _, err := f.fetch(t); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401 failure", err)
	}
	if f.Usable() {
		t.Fatal("a 401 must make the token unusable")
	}
	if _, err := f.fetch(t); !errors.Is(err, ErrTokenNotUsable) {
		t.Fatalf("after a 401 err = %v, want ErrTokenNotUsable", err)
	}
	if got := f.api.seen(); len(got) != 1 {
		t.Fatalf("requests = %v, want only the rejected one", got)
	}
}

func TestSnapshotTransportForbiddenFailsSnapshot(t *testing.T) {
	f := newSnapshotFixture(t)
	f.api.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/check-runs") {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource protected by organization SAML enforcement"}`))
		return true
	}
	_, err := f.fetch(t)
	var limited *ghsnapshot.RateLimitError
	if err == nil || errors.As(err, &limited) || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "SAML") {
		t.Fatalf("err = %v, want a plain 403 failure without the body", err)
	}
	if !f.Usable() {
		t.Fatal("a 403 on one repository must not stop the token")
	}
}

func TestSnapshotTransportNilService(t *testing.T) {
	if rt := (*Service)(nil).SnapshotTransport(); rt != nil {
		t.Fatalf("a nil Service must have no transport, got %T", rt)
	}
	if c := (*ghsnapshot.Client)(nil).WithStaticSource((*Service)(nil).SnapshotTransport()); c.Enabled() {
		t.Fatal("feature off must leave ghsnapshot disabled")
	}
}
