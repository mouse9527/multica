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
	// issues are the issues opened through the fake, by number, so their
	// labels and state can be read back and changed on "GitHub".
	issues map[int]*fakeGitHubIssue
}

// fakeGitHubIssue is an issue the fake keeps.
type fakeGitHubIssue struct {
	id      int
	title   string
	state   string
	labels  []string
	updated time.Time
}

// touch moves the issue's updated_at forward by at least a second, as the
// API reports it to the second.
func (i *fakeGitHubIssue) touch() {
	next := time.Now().UTC().Truncate(time.Second)
	if !next.After(i.updated) {
		next = i.updated.Add(time.Second)
	}
	i.updated = next
}

func (i *fakeGitHubIssue) json(number int) map[string]any {
	labels := make([]map[string]any, 0, len(i.labels))
	for _, name := range i.labels {
		labels = append(labels, map[string]any{"name": name})
	}
	return map[string]any{
		"id": i.id, "node_id": "I_x", "number": number, "title": i.title, "state": i.state,
		"labels": labels, "updated_at": i.updated.Format(time.RFC3339), "user": map[string]any{"login": "octocat"},
	}
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
	number, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
	f.mu.Lock()
	kept := f.issues[number]
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base+"/issues":
		issue := &fakeGitHubIssue{id: 90000 + id, title: stringOf(body["title"]), state: "open", labels: stringsOf(body["labels"])}
		issue.touch()
		f.mu.Lock()
		if f.issues == nil {
			f.issues = map[int]*fakeGitHubIssue{}
		}
		f.issues[300+id] = issue
		resp := issue.json(300 + id)
		f.mu.Unlock()
		resp["body"] = body["body"]
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && kept != nil && r.URL.Path == base+"/issues/"+strconv.Itoa(number):
		f.mu.Lock()
		resp := kept.json(number)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodPatch && kept != nil && r.URL.Path == base+"/issues/"+strconv.Itoa(number):
		f.mu.Lock()
		if title, ok := body["title"].(string); ok {
			kept.title = title
		}
		if state, ok := body["state"].(string); ok {
			kept.state = state
		}
		if _, ok := body["labels"]; ok {
			kept.labels = stringsOf(body["labels"])
		}
		kept.touch()
		resp := kept.json(number)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
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

// relabel changes an issue's labels on "GitHub", as someone editing them
// there would.
func (f *fakeWriteGitHub) relabel(number int, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[number].labels = labels
	f.issues[number].touch()
}

// labels returns an issue's labels on "GitHub".
func (f *fakeWriteGitHub) labels(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issues[number].labels...)
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, stringOf(item))
	}
	return out
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

// A stale edit is refused with the usual 409 before anything is sent to
// GitHub, so a later poll cannot copy it over the newer text. This is the
// content_base case: the comment is B, the client edits from A to C.
func TestGitHubStaleEditsSendNothing(t *testing.T) {
	h, fake, issueID := newGitHubWriteHandler(t)
	var created map[string]any
	testutil.Call(t, h.CreateComment, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": "A",
	}), "id", issueID)).Want(http.StatusCreated).JSON(&created)
	commentID := created["id"].(string)
	editComment := func(body map[string]any) *testutil.Response {
		return testutil.Call(t, h.UpdateComment, withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID, body), "commentId", commentID))
	}
	editComment(map[string]any{"content": "B"}).Want(http.StatusOK)
	fake.take()

	editComment(map[string]any{"content": "C", "content_base": "A"}).Want(http.StatusConflict)
	editComment(map[string]any{"content": "C", "expected_revision": 1}).Want(http.StatusConflict)
	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, commentID).Scan(&content)
	if requests, _ := fake.take(); len(requests) != 0 || content != "B" {
		t.Fatalf("stale comment edits sent %v, local content %q", requests, content)
	}

	updateIssue := func(body map[string]any) *testutil.Response {
		return testutil.Call(t, h.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, body), "id", issueID))
	}
	updateIssue(map[string]any{"title": "B"}).Want(http.StatusOK)
	fake.take()
	updateIssue(map[string]any{"title": "C", "title_base": "A"}).Want(http.StatusConflict)
	updateIssue(map[string]any{"title": "C", "expected_revision": 1}).Want(http.StatusConflict)
	var title string
	dbfx.QueryRow(t, `SELECT title FROM issue WHERE id = $1`, issueID).Scan(&title)
	if requests, _ := fake.take(); len(requests) != 0 || title != "B" {
		t.Fatalf("stale issue updates sent %v, local title %q", requests, title)
	}

	// The reconciler's delete of a comment GitHub already deleted runs the
	// delete path without sending anything to GitHub.
	comment, err := testHandler.Queries.GetComment(context.Background(), parseUUID(commentID))
	if err != nil {
		t.Fatal(err)
	}
	// DeleteComment's gate holds here too: a plain member may not delete
	// another member's comment, which is why the reconciler deletes as the
	// workspace owner (githubissues.deleteActor).
	plain := dbfx.User(t, "Plain member", "plain-"+strconv.FormatInt(time.Now().UnixNano(), 36)+"@example.com")
	dbfx.Member(t, testWorkspaceID, plain, "member")
	if err := h.DeleteGitHubComment(context.Background(), parseUUID(plain), comment); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("plain member deleting another's comment = %v, want 403", err)
	}
	if err := h.DeleteGitHubComment(context.Background(), parseUUID(testUserID), comment); err != nil {
		t.Fatalf("delete github comment: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND deleted_at IS NULL`, commentID); n != 0 {
		t.Fatalf("comment still live after the healed delete")
	}
	if requests, _ := fake.take(); len(requests) != 0 {
		t.Fatalf("healed delete sent %v", requests)
	}
	// Already gone counts as deleted.
	if err := h.DeleteGitHubComment(context.Background(), parseUUID(testUserID), comment); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// A create the local checks refuse (here the active-duplicate guard) never
// reaches GitHub: the request goes out only from inside the create
// transaction, after those checks.
func TestGitHubCreateRefusedLocallySendsNothing(t *testing.T) {
	h, fake, _ := newGitHubWriteHandler(t)
	create := func() *testutil.Response {
		return testutil.Call(t, h.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
			"title": "Opened twice", "repo": "acme/widgets", "status": "backlog",
		}))
	}
	var first map[string]any
	create().Want(http.StatusCreated).JSON(&first)
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, first["id"]) })
	fake.take()

	var dup map[string]any
	create().Want(http.StatusConflict).JSON(&dup)
	if dup["code"] != "active_duplicate_issue" {
		t.Fatalf("duplicate create = %v", dup)
	}
	if requests, _ := fake.take(); len(requests) != 0 {
		t.Fatalf("refused create sent %v", requests)
	}

	// GitHub refuses: the local create rolls back.
	fake.fail(http.StatusUnprocessableEntity)
	testutil.Call(t, h.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title": "Refused by GitHub", "repo": "acme/widgets", "status": "backlog",
	})).Want(http.StatusUnprocessableEntity)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = 'Refused by GitHub'`, testWorkspaceID); n != 0 {
		t.Fatalf("a create GitHub refused left %d local issues", n)
	}
}
