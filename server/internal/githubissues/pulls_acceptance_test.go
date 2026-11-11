package githubissues_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const acceptanceHead = "0123456789abcdef0123456789abcdef01234567"

// fakeGitHub serves one repository, acme/widgets (id 42), to both the poller
// and the PR snapshot pipeline.
type fakeGitHub struct {
	t     *testing.T
	mu    sync.Mutex
	pulls []map[string]any
}

func (f *fakeGitHub) openPull(number int, body string, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, map[string]any{
		"number": number, "html_url": "https://github.com/acme/widgets/pull/7",
		"title": "Handle empty config", "body": body, "state": "open", "draft": false,
		"merged_at": nil, "closed_at": nil,
		"created_at": updated.UTC().Format(time.RFC3339), "updated_at": updated.UTC().Format(time.RFC3339),
		"head": map[string]any{"ref": "empty-config", "sha": acceptanceHead},
		"user": map[string]any{"login": "contributor", "avatar_url": "https://avatars.example.com/contributor"},
		"base": map[string]any{"repo": map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}}},
	})
}

// acceptanceIssue is issue #1, opened on GitHub by someone else an hour ago.
func acceptanceIssue() map[string]any {
	return map[string]any{
		"node_id": "I_1", "number": 1, "title": "Crash on empty config", "body": "It crashes.",
		"state": "open", "state_reason": nil, "updated_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"user": map[string]any{"login": "stranger"},
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+githubissues.TestToken {
		f.t.Errorf("request without the token: %s", r.URL.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	firstPage := r.URL.Query().Get("page") == "1"
	var out any
	switch r.URL.Path {
	case "/repos/acme/widgets":
		out = map[string]any{"id": 42}
	case "/repositories/42/issues":
		out = []any{}
		if firstPage {
			out = []any{acceptanceIssue()}
		}
	case "/repositories/42/issues/1":
		out = acceptanceIssue()
	case "/repositories/42/issues/comments", "/repositories/42/issues/1/comments":
		out = []any{}
	case "/repositories/42/pulls":
		out = []any{}
		if firstPage {
			out = f.pulls
		}
	case "/repos/acme/widgets/pulls/7":
		out = map[string]any{"head": map[string]any{"sha": acceptanceHead}, "mergeable": true, "mergeable_state": "clean"}
	case "/repos/acme/widgets/commits/" + acceptanceHead + "/check-runs":
		out = map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{"id": 1, "head_sha": acceptanceHead, "name": "build", "status": "completed", "conclusion": "success"},
			{"id": 2, "head_sha": acceptanceHead, "name": "test", "status": "completed", "conclusion": "success"},
		}}
	case "/repos/acme/widgets/commits/" + acceptanceHead + "/status":
		out = map[string]any{"total_count": 0, "statuses": []any{}}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

// TestPolledPullRequestReachesIssueCard is the acceptance path of PR polling:
// a pull request opened on GitHub with "Closes #1" is on issue #1's PR card
// after the cycle that follows it, with its CI state, read with the server
// token through the poller, the PR mirror and the snapshot pipeline.
func TestPolledPullRequestReachesIssueCard(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	// The poller reads registered repositories across workspaces.
	pool := testutil.IsolatedDB(t, dsn)
	f := testutil.New(pool, "", "")
	suffix := uuid.NewString()[:8]
	owner := f.User(t, "Owner", "pulls-owner-"+suffix+"@example.com")
	ws := f.Workspace(t, "Pulls", "pulls-"+suffix)
	f.WorkspaceID, f.UserID = ws, owner
	f.Member(t, ws, owner, "owner")
	project := f.Project(t, "Widgets")
	f.Insert(t, "project_resource", testutil.Cols{
		"project_id": project, "workspace_id": ws,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/widgets"}`,
	})
	previous := issuekey.Enabled()
	issuekey.SetEnabled(true)
	t.Cleanup(func() { issuekey.SetEnabled(previous) })

	api := &fakeGitHub{t: t}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	bus := events.New()
	hub := realtime.NewHub()
	go hub.Run()
	svc := githubissues.NewServiceForTest(t, pool, bus, server.URL)
	h := handler.New(db.New(pool), pool, hub, bus, service.NewEmailService(), nil, nil, analytics.NoopClient{}, handler.Config{})
	if h.PRRefresh.Enabled() {
		t.Fatal("PR snapshots enabled before the static token source was wired")
	}
	h.UseGitHubIssuePulls(svc)
	if !h.PRRefresh.Enabled() || !h.TaskService.PRSnapshotsEnabled {
		t.Fatal("PR snapshots not enabled by the static token source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.PRRefresh.Start(ctx)

	poll := func() {
		t.Helper()
		stats, err := svc.PollOnce(ctx)
		if err != nil || stats.Failed != 0 {
			t.Fatalf("poll: %+v %v", stats, err)
		}
	}
	wsID, err := util.ParseUUID(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ImportIssue(ctx, wsID, githubissues.IssueRef{Owner: "acme", Repo: "widgets", Number: 1}); err != nil {
		t.Fatalf("import #1: %v", err)
	}
	poll()
	var issueID string
	f.QueryRow(t, `SELECT issue_id::text FROM issue_external WHERE workspace_id = $1 AND external_number = 1`, ws).Scan(&issueID)
	if issueID == "" {
		t.Fatal("issue #1 not imported")
	}

	// Someone opens a pull request closing #1; the next cycle brings it in.
	api.openPull(7, "Fixes the crash.\n\nCloses #1", time.Now().Add(-time.Minute))
	poll()

	listPRs := func() []handler.GitHubPullRequestResponse {
		t.Helper()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", issueID)
		req := httptest.NewRequest(http.MethodGet, "/api/issues/"+issueID+"/pull-requests", nil)
		req.Header.Set("X-User-ID", owner)
		req.Header.Set("X-Workspace-ID", ws)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		h.ListPullRequestsForIssue(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("list pull requests: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			PullRequests []handler.GitHubPullRequestResponse `json:"pull_requests"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.PullRequests
	}
	prs := listPRs()
	if len(prs) != 1 || prs[0].Number != 7 || prs[0].State != "open" ||
		prs[0].RepoOwner != "acme" || prs[0].RepoName != "widgets" || prs[0].LinkSource != "auto" {
		t.Fatalf("issue #1 pull requests = %+v, want acme/widgets#7 linked automatically", prs)
	}

	// The snapshot is fetched in the background with the static source.
	deadline := time.Now().Add(10 * time.Second)
	for {
		pr := listPRs()[0]
		if pr.ChecksConclusion != nil {
			if *pr.ChecksConclusion != "passed" || pr.ChecksTotal != 2 || pr.ChecksPassed != 2 {
				t.Fatalf("CI state = %s %d/%d", *pr.ChecksConclusion, pr.ChecksPassed, pr.ChecksTotal)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CI state never arrived: %+v", pr)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Another cycle with nothing new writes nothing.
	poll()
	var rows int
	f.QueryRow(t, `SELECT count(*) FROM github_pull_request WHERE workspace_id = $1`, ws).Scan(&rows)
	if rows != 1 {
		t.Fatalf("github_pull_request rows = %d, want 1", rows)
	}
}
