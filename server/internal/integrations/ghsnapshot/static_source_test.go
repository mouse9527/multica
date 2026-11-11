package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// staticCredential stands in for what the injected transport adds; the
// client itself never holds it.
const staticCredential = "Bearer static-test-credential"

// credentialTransport plays the transport githubissues injects: it adds the
// credentials and counts the requests it carried.
type credentialTransport struct {
	calls atomic.Int32
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", staticCredential)
	return http.DefaultTransport.RoundTrip(out)
}

// staticSnapshotServer serves a PR whose head has two failing Actions suites,
// so FetchPRSnapshot also reads the workflow runs. Every request must carry
// the static credential; minting an installation token fails the test.
func staticSnapshotServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var actionsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != staticCredential || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if handler != nil && handler(w, r) {
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/pulls/9":
			writeTestPR(w, "sha9", true, "clean")
		case "/repos/o/r/commits/sha9/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
				{"id": 1, "head_sha": "sha9", "name": "test", "status": "completed", "conclusion": "cancelled", "app": map[string]any{"slug": "github-actions"}, "check_suite": map[string]any{"id": 10}},
				{"id": 2, "head_sha": "sha9", "name": "test", "status": "completed", "conclusion": "success", "app": map[string]any{"slug": "github-actions"}, "check_suite": map[string]any{"id": 20}},
			}})
		case "/repos/o/r/commits/sha9/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "statuses": []map[string]any{{"context": "deploy", "state": "success"}}})
		case "/repos/o/r/actions/runs":
			actionsCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "workflow_runs": []map[string]any{
				{"id": 100, "workflow_id": 5, "check_suite_id": 10, "head_sha": "sha9", "event": "push", "status": "completed", "conclusion": "cancelled"},
				{"id": 101, "workflow_id": 5, "check_suite_id": 20, "head_sha": "sha9", "event": "push", "status": "completed", "conclusion": "success"},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &actionsCalls
}

func TestFetchPRSnapshotWithStaticSource(t *testing.T) {
	srv, actionsCalls := staticSnapshotServer(t, nil)
	transport := &credentialTransport{}
	c := (*Client)(nil).WithStaticSource(transport)
	if !c.Enabled() {
		t.Fatal("a client with only a static source must be enabled")
	}
	c.apiBase = srv.URL

	snap, err := FetchPRSnapshot(context.Background(), c, StaticInstallationID, "o", "r", 9)
	if err != nil {
		t.Fatal(err)
	}
	// The cancelled suite was superseded by the newer run of the same
	// workflow, which needs the Actions read the static token may always try.
	if snap.HeadSHA != "sha9" || snap.Mergeable != "MERGEABLE" || snap.RollupState != "SUCCESS" || len(snap.Contexts) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := actionsCalls.Load(); got != 1 {
		t.Fatalf("workflow runs read %d times, want 1", got)
	}
	// PR, check runs, status, workflow runs, PR again.
	if got := transport.calls.Load(); got != 5 {
		t.Fatalf("transport carried %d requests, want 5", got)
	}
	if len(c.tokens) != 0 {
		t.Fatalf("static source cached installation tokens: %d", len(c.tokens))
	}
}

func TestStaticSourceKeepsAppPath(t *testing.T) {
	var appCalls, staticCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/app/installations/1/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_secret", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case r.Header.Get("Authorization") == "Bearer ghs_secret":
			appCalls.Add(1)
			writeTestPR(w, "app-sha", true, "clean")
		case r.Header.Get("Authorization") == staticCredential:
			staticCalls.Add(1)
			writeTestPR(w, "static-sha", true, "clean")
		default:
			t.Errorf("request without known credentials: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	transport := &credentialTransport{}
	c := newTestClient(t, srv.URL).WithStaticSource(transport)

	app, err := fetchSnapshotPR(context.Background(), c, 1, "/repos/o/r/pulls/1")
	if err != nil || app.Head.SHA != "app-sha" {
		t.Fatalf("App path = %+v, %v", app, err)
	}
	static, err := fetchSnapshotPR(context.Background(), c, StaticInstallationID, "/repos/o/r/pulls/1")
	if err != nil || static.Head.SHA != "static-sha" {
		t.Fatalf("static path = %+v, %v", static, err)
	}
	if appCalls.Load() != 1 || staticCalls.Load() != 1 || transport.calls.Load() != 1 {
		t.Fatalf("app=%d static=%d via transport=%d", appCalls.Load(), staticCalls.Load(), transport.calls.Load())
	}
	if c.tokenCanRead(1, "actions") {
		t.Fatal("the App token's permissions must still decide for an installation")
	}
	if !c.tokenCanRead(StaticInstallationID, "actions") {
		t.Fatal("the static token must always be allowed to try")
	}
}

func TestStaticSourceRefusals(t *testing.T) {
	t.Run("403 fails the snapshot", func(t *testing.T) {
		srv, _ := staticSnapshotServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/repos/o/r/actions/runs" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
				return true
			}
			return false
		})
		c := (*Client)(nil).WithStaticSource(&credentialTransport{})
		c.apiBase = srv.URL
		_, err := FetchPRSnapshot(context.Background(), c, StaticInstallationID, "o", "r", 9)
		var rl *RateLimitError
		if err == nil || errors.As(err, &rl) || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err = %v, want a plain 403 failure", err)
		}
		if strings.Contains(err.Error(), "Resource not accessible") || strings.Contains(err.Error(), "static-test-credential") {
			t.Fatalf("error leaks the body or credentials: %v", err)
		}
	})

	t.Run("no static source", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("no request may be sent: %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		c := newTestClient(t, srv.URL)
		if _, err := c.restGet(context.Background(), StaticInstallationID, "/repos/o/r/pulls/1"); err == nil {
			t.Fatal("the static id must not fall back to the App")
		}
	})

	t.Run("no App", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("no request may be sent: %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		c := (*Client)(nil).WithStaticSource(&credentialTransport{})
		c.apiBase = srv.URL
		if _, err := c.restGet(context.Background(), 1, "/repos/o/r/pulls/1"); err == nil {
			t.Fatal("an installation id must not use the static source")
		}
	})

	t.Run("nil transport", func(t *testing.T) {
		if c := (*Client)(nil).WithStaticSource(nil); c.Enabled() {
			t.Fatal("no App and no static source must stay disabled")
		}
	})
}
