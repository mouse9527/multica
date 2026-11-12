// Added by the mouse9527/multica fork: syncing GitHub issues on request.

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// POST /api/issues/{id}/github-sync syncs a GitHub issue named by key or id
// and answers what changed; it refuses a local issue, and answers 429 with
// the wait when the quota is below the guard. GET
// /api/projects/{id}/github-issues lists what a project sync reads.
func TestSyncGitHubIssue(t *testing.T) {
	fake := &fakeImportGitHub{}
	var remaining atomic.Int64
	remaining.Store(4000)
	reset := time.Now().Add(30 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining.Load(), 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	svc := githubissues.New(githubissues.LoadConfig(func(key string) string {
		return map[string]string{
			githubissues.EnvEnabled:        "true",
			githubissues.EnvToken:          fakeGitHubToken,
			githubissues.EnvTokenExpiresAt: "never",
		}[key]
	}), githubissues.Options{DB: testPool, APIBase: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.Start(ctx)
	for deadline := time.Now().Add(5 * time.Second); !svc.Usable(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("token never accepted by the fake GitHub")
		}
	}
	previous := issuekey.Enabled()
	issuekey.SetEnabled(true)
	t.Cleanup(func() { issuekey.SetEnabled(previous) })

	projectID := dbfx.Project(t, "GitHub syncs")
	dbfx.Insert(t, "project_resource", testutil.Cols{
		"project_id": projectID, "workspace_id": testWorkspaceID,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/widgets"}`,
	})
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{
			`DELETE FROM comment WHERE issue_id IN (SELECT issue_id FROM issue_external WHERE workspace_id = $1 AND external_repo_id = ` + strconv.Itoa(importTestRepoID) + `)`,
			`DELETE FROM issue WHERE id IN (SELECT issue_id FROM issue_external WHERE workspace_id = $1 AND external_repo_id = ` + strconv.Itoa(importTestRepoID) + `)`,
			`DELETE FROM comment_external WHERE workspace_id = $1`,
			`DELETE FROM issue_external WHERE workspace_id = $1`,
			`DELETE FROM github_repo_sync WHERE workspace_id = $1`,
		} {
			_, _ = testPool.Exec(ctx, sql, testWorkspaceID)
		}
	})
	h := *testHandler
	h.GitHubIssues = svc

	var imported IssueResponse
	testutil.Call(t, h.ImportGitHubIssue, newRequest(http.MethodPost, "/api/issues/import", map[string]any{"ref": "acme/widgets#1"})).
		Want(http.StatusCreated).JSON(&imported)

	syncRef := func(ref string) *testutil.Response {
		req := newRequest(http.MethodPost, "/api/issues/"+url.PathEscape(ref)+"/github-sync", nil)
		return testutil.Call(t, h.SyncGitHubIssue, withURLParam(req, "id", ref))
	}

	var got GitHubSyncResponse
	syncRef("acme/widgets#1").Want(http.StatusOK).JSON(&got)
	if got.Identifier != "acme/widgets#1" || got.IssueID != imported.ID || got.RequestsUsed != 2 ||
		got.NewComments != 0 || got.RateLimit == nil || got.RateLimit.Remaining != 4000 {
		t.Fatalf("sync = %+v", got)
	}
	syncRef(imported.ID).Want(http.StatusOK)
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, imported.ID); n != 1 {
		t.Fatalf("comments after syncs = %d, want 1", n)
	}

	var list struct {
		Issues []ProjectGitHubIssueResponse `json:"issues"`
	}
	listReq := newRequest(http.MethodGet, "/api/projects/"+projectID+"/github-issues?open_only=true", nil)
	testutil.Call(t, h.ListProjectGitHubIssues, withURLParam(listReq, "id", projectID)).Want(http.StatusOK).JSON(&list)
	if len(list.Issues) != 1 || list.Issues[0].ID != imported.ID || list.Issues[0].Identifier != "acme/widgets#1" || list.Issues[0].State != "open" {
		t.Fatalf("project issues = %+v", list.Issues)
	}

	local := dbfx.Issue(t, "Local only")
	var refused map[string]any
	syncRef(local).Want(http.StatusUnprocessableEntity).JSON(&refused)
	if refused["code"] != "github_issue_local" {
		t.Fatalf("local issue = %v", refused)
	}

	// The quota drops under the guard: the next sync stops after its first
	// read, which reports it, and says how far it got; the one after that
	// sends nothing.
	remaining.Store(10)
	sent := fake.sent()
	var stopped map[string]any
	syncRef(imported.ID).Want(http.StatusTooManyRequests).JSON(&stopped)
	if stopped["code"] != "github_quota_low" || stopped["requests_used"] != float64(1) || stopped["issue_id"] != imported.ID ||
		stopped["new_comments"] != float64(0) || fake.sent() != sent+1 {
		t.Fatalf("sync when the quota drops = %v after %d requests", stopped, fake.sent()-sent)
	}
	sent = fake.sent()
	resp := syncRef(imported.ID).Want(http.StatusTooManyRequests)
	var low map[string]any
	resp.JSON(&low)
	if low["code"] != "github_quota_low" || low["remaining"] != float64(10) || low["threshold"] != float64(1000) ||
		low["retry_after_seconds"] == nil || resp.Header().Get("Retry-After") == "" || fake.sent() != sent {
		t.Fatalf("sync below the guard = %v, Retry-After %q, %d requests", low, resp.Header().Get("Retry-After"), fake.sent()-sent)
	}
}
