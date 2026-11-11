// Modified for the mouse9527/multica fork: GitHub-native issues.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const writeTestRepoID = 4242

// fakeWriteGitHub is a GitHub API that accepts the writes the handler hooks
// send, records them, and fails the next one on request.
type fakeWriteGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	failNext int
	nextID   int
}

func newFakeWriteGitHub(t *testing.T) *fakeWriteGitHub {
	f := &fakeWriteGitHub{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeWriteGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fakeGitHubToken {
		f.t.Errorf("request without the token: %s", r.URL.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/user":
		w.Header().Set("X-OAuth-Scopes", "repo")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
		return
	case "/repos/acme/widgets":
		_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(writeTestRepoID) + `}`))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, body)
	fail := f.failNext
	f.failNext = 0
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	if fail != 0 {
		w.WriteHeader(fail)
		_, _ = w.Write([]byte(`{"message":"failure"}`))
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	base := "/repositories/" + strconv.Itoa(writeTestRepoID)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base+"/issues":
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 90000 + id, "node_id": "I_x", "number": 300 + id, "title": body["title"], "body": body["body"],
			"state": "open", "updated_at": now, "user": map[string]any{"login": "octocat"},
		})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/issues/comments/"),
		r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		commentID := int64(80000 + id)
		if r.Method == http.MethodPatch {
			commentID, _ = strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": commentID, "body": body["body"], "updated_at": now, "user": map[string]any{"login": "octocat"},
		})
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/issues/"):
		n, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 70000 + n, "node_id": "I_y", "number": n, "title": body["title"], "state": "open",
			"updated_at": now, "user": map[string]any{"login": "octocat"},
		})
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeWriteGitHub) take() ([]string, []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests, bodies := f.requests, f.bodies
	f.requests, f.bodies = nil, nil
	return requests, bodies
}

func (f *fakeWriteGitHub) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = status
}

// newGitHubWriteHandler returns the shared test handler with GitHub issues on
// against the fake, acme/widgets registered in the test workspace, and one
// GitHub issue (#5) linked to a local issue.
func newGitHubWriteHandler(t *testing.T) (*Handler, *fakeWriteGitHub, string) {
	t.Helper()
	fake := newFakeWriteGitHub(t)
	svc := githubissues.New(githubissues.LoadConfig(func(key string) string {
		return map[string]string{
			githubissues.EnvEnabled:        "true",
			githubissues.EnvToken:          fakeGitHubToken,
			githubissues.EnvTokenExpiresAt: "never",
		}[key]
	}), githubissues.Options{DB: testPool, APIBase: fake.server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.Start(ctx)
	for deadline := time.Now().Add(5 * time.Second); !svc.Usable(); {
		if time.Now().After(deadline) {
			t.Fatal("token never accepted by the fake GitHub")
		}
		time.Sleep(10 * time.Millisecond)
	}
	previous := issuekey.Enabled()
	issuekey.SetEnabled(true)
	t.Cleanup(func() { issuekey.SetEnabled(previous) })

	projectID := dbfx.Project(t, "GitHub writes")
	dbfx.Insert(t, "project_resource", testutil.Cols{
		"project_id": projectID, "workspace_id": testWorkspaceID,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/widgets"}`,
	})
	issueID := dbfx.Issue(t, "Linked issue", testutil.Cols{"project_id": projectID})
	dbfx.Exec(t, `INSERT INTO issue_external (issue_id, workspace_id, provider, external_repo_id, external_repo,
		external_number, external_node_id, state, external_updated_at)
		VALUES ($1, $2, 'github', $3, 'acme/widgets', 5, 'I_5', 'open', now() - interval '1 hour')`,
		issueID, testWorkspaceID, writeTestRepoID)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{
			`DELETE FROM comment_external WHERE workspace_id = $1`,
			`DELETE FROM issue_external WHERE workspace_id = $1`,
			`DELETE FROM github_write_intent WHERE workspace_id = $1`,
			`DELETE FROM github_repo_sync WHERE workspace_id = $1`,
		} {
			_, _ = testPool.Exec(ctx, sql, testWorkspaceID)
		}
	})
	h := *testHandler
	h.ExternalComments = svc
	h.GitHubIssues = svc
	return &h, fake, issueID
}

// The handler hooks send each write of a GitHub issue to GitHub before the
// local write, refuse the local write when GitHub did not take it, and send
// nothing for fields only Multica has.
func TestGitHubIssueWritesGoThroughGitHub(t *testing.T) {
	h, fake, issueID := newGitHubWriteHandler(t)
	updateIssue := func(body map[string]any) *testutil.Response {
		return testutil.Call(t, h.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, body), "id", issueID))
	}
	title := func() string {
		var got string
		dbfx.QueryRow(t, `SELECT title FROM issue WHERE id = $1`, issueID).Scan(&got)
		return got
	}

	updateIssue(map[string]any{"title": "Renamed"}).Want(http.StatusOK)
	requests, bodies := fake.take()
	if len(requests) != 1 || requests[0] != "PATCH /repositories/4242/issues/5" || bodies[0]["title"] != "Renamed" {
		t.Fatalf("GitHub requests = %v %v", requests, bodies)
	}
	if title() != "Renamed" {
		t.Fatalf("local title = %q", title())
	}

	// GitHub's answer is lost: the caller is told, nothing is written
	// locally, and nothing is re-sent.
	fake.fail(http.StatusBadGateway)
	var unconfirmed map[string]any
	updateIssue(map[string]any{"title": "Lost"}).Want(http.StatusBadGateway).JSON(&unconfirmed)
	if unconfirmed["code"] != "github_write_unconfirmed" || title() != "Renamed" {
		t.Fatalf("unconfirmed write = %v, local title %q", unconfirmed, title())
	}
	// GitHub rejects the write.
	fake.fail(http.StatusUnprocessableEntity)
	updateIssue(map[string]any{"title": "Rejected"}).Want(http.StatusUnprocessableEntity)
	if requests, _ := fake.take(); len(requests) != 2 || title() != "Renamed" {
		t.Fatalf("failed writes sent %v, local title %q", requests, title())
	}

	// Priority is Multica's own: written locally, nothing sent.
	updateIssue(map[string]any{"priority": "high"}).Want(http.StatusOK)
	if requests, _ := fake.take(); len(requests) != 0 {
		t.Fatalf("priority change sent %v", requests)
	}

	// Comments: posted, linked, edited, deleted.
	var created map[string]any
	testutil.Call(t, h.CreateComment, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": "On it",
	}), "id", issueID)).Want(http.StatusCreated).JSON(&created)
	commentID, _ := created["id"].(string)
	requests, bodies = fake.take()
	if len(requests) != 1 || requests[0] != "POST /repositories/4242/issues/5/comments" ||
		!strings.HasPrefix(bodies[0]["body"].(string), "On it\n\n<!-- multica:v1 actor=member:"+testUserID+" op=") {
		t.Fatalf("comment request = %v %v", requests, bodies)
	}
	var externalID int64
	dbfx.QueryRow(t, `SELECT external_id FROM comment_external WHERE comment_id = $1`, commentID).Scan(&externalID)
	if externalID == 0 {
		t.Fatalf("created comment %s not linked to GitHub", commentID)
	}
	testutil.Call(t, h.UpdateComment, withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{
		"content": "Done",
	}), "commentId", commentID)).Want(http.StatusOK)
	testutil.Call(t, h.DeleteComment, withURLParam(newRequest(http.MethodDelete, "/api/comments/"+commentID, nil), "commentId", commentID)).
		Want(http.StatusNoContent)
	requests, _ = fake.take()
	path := "/repositories/4242/issues/comments/" + strconv.FormatInt(externalID, 10)
	if len(requests) != 2 || requests[0] != "PATCH "+path || requests[1] != "DELETE "+path {
		t.Fatalf("edit/delete requests = %v", requests)
	}

	// A new issue for a GitHub repository is opened there and linked in the
	// create transaction.
	var issue map[string]any
	testutil.Call(t, h.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title": "Opened from Multica", "repo": "acme/widgets", "status": "backlog",
	})).Want(http.StatusCreated).JSON(&issue)
	if requests, _ := fake.take(); len(requests) != 1 || requests[0] != "POST /repositories/4242/issues" {
		t.Fatalf("create requests = %v", requests)
	}
	if issue["provider"] != "github" || !strings.HasPrefix(issue["identifier"].(string), "acme/widgets#") {
		t.Fatalf("created issue = %v", issue)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue["id"]) })
	if n := dbfx.Count(t, `SELECT count(*) FROM github_write_intent WHERE workspace_id = $1 AND state = 'applied'`, testWorkspaceID); n != 5 {
		t.Fatalf("applied intents = %d, want 5", n)
	}
}

// With the feature off the repo field is ignored and nothing is sent, as
// upstream ignores fields it does not know.
func TestGitHubIssueWritesOffMatchesUpstream(t *testing.T) {
	if testHandler.GitHubIssues != nil {
		t.Fatal("the shared test handler must not carry the fork hook")
	}
	var issue map[string]any
	testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title": "Repo ignored while off", "repo": "acme/widgets",
	})).Want(http.StatusCreated).JSON(&issue)
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue["id"]) })
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_external WHERE issue_id = $1`, issue["id"]); n != 0 || issue["provider"] != "local" {
		t.Fatalf("issue created while off = %v", issue)
	}
}
