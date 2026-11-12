// Added by the mouse9527/multica fork: importing GitHub issues on request.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const importTestRepoID = 4343

// fakeImportGitHub serves acme/widgets with issue #1 (one comment), pull
// request #2 and issue #3 whose comments GitHub fails to list, and records
// the requests to the repository.
type fakeImportGitHub struct {
	mu       sync.Mutex
	requests []string
}

func (f *fakeImportGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	base := "/repositories/" + strconv.Itoa(importTestRepoID)
	updated := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	switch r.URL.Path {
	case "/user":
		w.Header().Set("X-OAuth-Scopes", "repo")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
		return
	case "/repos/acme/widgets":
		_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(importTestRepoID) + `}`))
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()
	switch r.URL.Path {
	case base + "/issues/1":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1001, "node_id": "I_1", "number": 1, "title": "Crash on start", "body": "It crashes.",
			"state": "open", "updated_at": updated, "user": map[string]any{"login": "stranger"},
		})
	case base + "/issues/1/comments":
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 501, "body": "Same here", "updated_at": updated,
			"user": map[string]any{"login": "stranger", "avatar_url": "https://avatars.example.com/stranger"},
		}})
	case base + "/issues/3":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1003, "node_id": "I_3", "number": 3, "title": "Docs typo", "body": "",
			"state": "open", "updated_at": updated, "user": map[string]any{"login": "stranger"},
		})
	case base + "/issues/3/comments":
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"failure"}`))
	case base + "/issues/2":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1002, "node_id": "PR_2", "number": 2, "title": "Fix crash", "body": "",
			"state": "open", "updated_at": updated, "user": map[string]any{"login": "stranger"},
			"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widgets/pulls/2"},
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func (f *fakeImportGitHub) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// POST /api/issues/import stores a GitHub issue of a registered repository
// once, answers the same issue on a second import, and refuses anything that
// is not an issue of a registered repository with a code the CLI can show.
func TestImportGitHubIssue(t *testing.T) {
	fake := &fakeImportGitHub{}
	server := httptest.NewServer(fake)
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

	projectID := dbfx.Project(t, "GitHub imports")
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

	importRef := func(ref string) *testutil.Response {
		return testutil.Call(t, h.ImportGitHubIssue, newRequest(http.MethodPost, "/api/issues/import", map[string]any{"ref": ref}))
	}

	var first IssueResponse
	importRef("https://github.com/acme/widgets/issues/1").Want(http.StatusCreated).JSON(&first)
	if first.Identifier != "acme/widgets#1" || first.Provider != "github" || first.ProjectID == nil || *first.ProjectID != projectID {
		t.Fatalf("imported issue = %+v", first)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, first.ID); n != 1 {
		t.Fatalf("imported comments = %d, want 1", n)
	}
	sent := fake.sent()
	var again IssueResponse
	importRef("acme/widgets#1").Want(http.StatusOK).JSON(&again)
	// Only the comments are read again.
	if again.ID != first.ID || fake.sent() != sent+1 {
		t.Fatalf("second import = %s after %d requests, want %s after one comments read", again.ID, fake.sent()-sent, first.ID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, first.ID); n != 1 {
		t.Fatalf("comments after the second import = %d, want 1", n)
	}

	for _, c := range []struct {
		ref    string
		status int
		code   string
	}{
		{"not a reference", http.StatusBadRequest, ""},
		{"https://github.com/acme/widgets/pull/2", http.StatusUnprocessableEntity, "github_pull_request"},
		{"acme/widgets#2", http.StatusUnprocessableEntity, "github_pull_request"},
		{"acme/secrets#1", http.StatusForbidden, "github_repo_not_registered"},
		{"acme/widgets#99", http.StatusNotFound, "github_issue_not_found"},
	} {
		var body map[string]any
		importRef(c.ref).Want(c.status).JSON(&body)
		if c.code != "" && body["code"] != c.code {
			t.Errorf("%s: body = %v, want code %s", c.ref, body, c.code)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_external WHERE workspace_id = $1 AND external_repo_id = $2`,
		testWorkspaceID, importTestRepoID); n != 1 {
		t.Fatalf("issue_external rows = %d, want only the first import", n)
	}

	// The issue is stored but its comments could not be read: the answer
	// says so and names the issue, so the client imports again to finish.
	var incomplete map[string]any
	importRef("acme/widgets#3").Want(http.StatusBadGateway).JSON(&incomplete)
	id, _ := incomplete["issue_id"].(string)
	if incomplete["code"] != "github_import_incomplete" || id == "" {
		t.Fatalf("incomplete import = %v", incomplete)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_external WHERE issue_id = $1`, id); n != 1 {
		t.Fatalf("issue of the incomplete import not stored")
	}

	// Once the project is private, a workspace member outside it gets the
	// answer for an unregistered repository on a new import, a re-import and
	// an attempt to finish the incomplete one; nothing is sent or stored.
	dbfx.Exec(t, `UPDATE project SET visibility = 'private' WHERE id = $1`, projectID)
	outsider := createPlainMember(t, fmt.Sprintf("import-outsider-%d@multica.test", time.Now().UnixNano()))
	sent = fake.sent()
	for _, ref := range []string{"acme/widgets#5", "acme/widgets#1", "acme/widgets#3"} {
		var body map[string]any
		testutil.Call(t, h.ImportGitHubIssue, asUser(newRequest(http.MethodPost, "/api/issues/import", map[string]any{"ref": ref}), outsider)).
			Want(http.StatusForbidden).JSON(&body)
		if body["code"] != "github_repo_not_registered" || body["issue_id"] != nil || body["title"] != nil {
			t.Errorf("outsider import %s: body = %v", ref, body)
		}
	}
	if fake.sent() != sent {
		t.Fatalf("outsider imports sent %d requests", fake.sent()-sent)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_external WHERE workspace_id = $1 AND external_repo_id = $2`,
		testWorkspaceID, importTestRepoID); n != 2 {
		t.Fatalf("issue_external rows = %d after outsider imports, want 2", n)
	}
	// The owner still sees the private project and re-imports normally.
	importRef("acme/widgets#1").Want(http.StatusOK)
}
