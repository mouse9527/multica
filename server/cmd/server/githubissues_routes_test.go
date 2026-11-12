// Added by the mouse9527/multica fork: route wiring of GitHub-native issues.

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// routeTestToken is shaped like a classic PAT but is not a real credential.
const routeTestToken = "ghp_FAKEtokenFORrouteTESTS0123456789abcd"

func tokenStatusPath() string {
	return "/api/workspaces/" + testWorkspaceID + "/github-issues/token-status"
}

func getTokenStatus(t *testing.T, serverURL, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, serverURL+tokenStatusPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// With the feature off the route does not exist, as upstream.
func TestGitHubTokenStatusRouteAbsentWhileOff(t *testing.T) {
	if code, _ := getTokenStatus(t, testServer.URL, testToken); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// With the feature off POST /api/issues/import answers like any other
// unknown path under /api/issues, as upstream.
func TestGitHubIssueImportRouteAbsentWhileOff(t *testing.T) {
	post := func(path string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, testServer.URL+path, strings.NewReader(`{"ref":"acme/widgets#1"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	got, upstream := post("/api/issues/import"), post("/api/issues/no-such-route")
	if got != upstream || got < 400 {
		t.Fatalf("import while off = %d, want %d like any unknown path", got, upstream)
	}
}

// With the feature off the sync routes answer like any other unknown path
// under an issue or a project, as upstream.
func TestGitHubSyncRoutesAbsentWhileOff(t *testing.T) {
	call := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, testServer.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	const id = "00000000-0000-4000-8000-000000000001"
	for _, c := range []struct{ method, route, unknown string }{
		{http.MethodPost, "/api/issues/" + id + "/github-sync", "/api/issues/" + id + "/no-such-route"},
		{http.MethodGet, "/api/projects/" + id + "/github-issues", "/api/projects/" + id + "/no-such-route"},
	} {
		if got, upstream := call(c.method, c.route), call(c.method, c.unknown); got != upstream || got < 400 {
			t.Errorf("%s %s while off = %d, want %d like any unknown path", c.method, c.route, got, upstream)
		}
	}
}

func TestGitHubTokenStatusRouteIsAdminOnly(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "repo")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(github.Close)
	env := map[string]string{
		githubissues.EnvEnabled:        "true",
		githubissues.EnvToken:          routeTestToken,
		githubissues.EnvTokenExpiresAt: "never",
	}
	svc := githubissues.New(githubissues.LoadConfig(func(k string) string { return env[k] }),
		githubissues.Options{APIBase: github.URL})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.Start(ctx)
	for deadline := time.Now().Add(5 * time.Second); !svc.Usable(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("token was not validated against the fake GitHub")
		}
	}

	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil,
		RouterOptions{GitHubIssues: svc})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	code, body := getTokenStatus(t, server.URL, testToken)
	if code != http.StatusOK {
		t.Fatalf("owner: status = %d, body %s", code, body)
	}
	if strings.Contains(body, routeTestToken[:12]) {
		t.Fatalf("response leaks the token: %s", body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["status"] != "ok" || got["account_login"] != "octocat" {
		t.Fatalf("response = %v", got)
	}

	f := testutil.New(testPool, testWorkspaceID, testUserID)
	suffix := uuid.NewString()[:8]
	email := "gh-status-member-" + suffix + "@example.invalid"
	member := f.User(t, "Plain member", email)
	f.Member(t, testWorkspaceID, member, "member")
	memberToken, err := generateTestJWT(member, email, "Plain member")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := getTokenStatus(t, server.URL, memberToken); code != http.StatusForbidden {
		t.Fatalf("plain member: status = %d, want 403", code)
	}
}
