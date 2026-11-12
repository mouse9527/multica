package githubissues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// fakeRepo is another repository of the fake GitHub, besides acme/widgets.
type fakeRepo struct {
	name   string
	id     int64
	issues map[int]map[string]any
}

func fakeRepoIssue(number int, updated time.Time) map[string]any {
	return map[string]any{
		"id": 3000 + number, "node_id": fmt.Sprintf("I_%d", number), "number": number,
		"title": fmt.Sprintf("Issue %d", number), "body": "body", "state": "open", "state_reason": nil,
		"updated_at": updated.UTC().Format(time.RFC3339), "user": map[string]any{"login": "stranger"},
	}
}

// registerRepo registers acme/name on the test project.
func (env *pollEnv) registerRepo(t *testing.T, name string) {
	t.Helper()
	env.f.Insert(t, "project_resource", testutil.Cols{
		"project_id": env.project, "workspace_id": env.ws,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/` + name + `"}`,
	})
}

// serveRepos registers the repositories and serves them: each resolves to
// its id, its lists have nothing new, and its issues read as given. Any other
// repository is unknown to the fake, so a request to it fails the test.
func (env *pollEnv) serveRepos(t *testing.T, repos ...fakeRepo) {
	t.Helper()
	for _, repo := range repos {
		env.registerRepo(t, repo.name)
	}
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	env.api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		for _, repo := range repos {
			base := fmt.Sprintf("/repositories/%d/issues", repo.id)
			switch path := r.URL.Path; {
			case path == "/repos/acme/"+repo.name:
				_, _ = fmt.Fprintf(w, `{"id":%d}`, repo.id)
			case path == base, path == base+"/comments", strings.HasSuffix(path, "/comments") && strings.HasPrefix(path, base+"/"):
				_, _ = w.Write([]byte(`[]`))
			case strings.HasPrefix(path, base+"/"):
				var n int
				if _, err := fmt.Sscanf(strings.TrimPrefix(path, base+"/"), "%d", &n); err != nil {
					return false
				}
				issue, ok := repo.issues[n]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return true
				}
				_ = json.NewEncoder(w).Encode(issue)
			default:
				continue
			}
			return true
		}
		return false
	}
}

// requestsByRepo counts the requests since from by repository id, and
// reports any request that names no repository the test expects.
func (env *pollEnv) requestsByRepo(from int) map[int64]int {
	out := map[int64]int{}
	for _, req := range env.api.seen()[from:] {
		path, _, _ := strings.Cut(req, "?")
		path = strings.TrimPrefix(strings.TrimPrefix(path, "GET "), "POST ")
		var id int64
		if _, err := fmt.Sscanf(path, "/repositories/%d/", &id); err == nil {
			out[id]++
			continue
		}
		out[-1]++ // anything else, such as resolving a name
	}
	return out
}

// TestPollFollowsOnlyRepositoriesWithTrackedIssues is #57's acceptance test:
// of 20 registered repositories only the 2 with tracked issues get requests
// in a cycle; importing an issue of a third makes it followed from the next
// cycle; a repository whose last tracked issue is gone drops out; and a write
// still to settle keeps a repository followed until it is settled or handed
// over.
func TestPollFollowsOnlyRepositoriesWithTrackedIssues(t *testing.T) {
	env := newPollEnv(t)
	ctx := context.Background()
	ws := util.MustParseUUID(env.ws)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Issue 1", "body", "open", t0, nil)
	gadgets := fakeRepo{name: "gadgets", id: 43, issues: map[int]map[string]any{5: fakeRepoIssue(5, t0)}}
	third := fakeRepo{name: "third", id: 44, issues: map[int]map[string]any{9: fakeRepoIssue(9, t0)}}
	env.serveRepos(t, gadgets, third)
	for i := 1; i <= 17; i++ {
		env.registerRepo(t, fmt.Sprintf("quiet-%02d", i))
	}
	if n := env.count(t, `SELECT count(*) FROM project_resource WHERE workspace_id = $1`, env.ws); n != 20 {
		t.Fatalf("registered repositories = %d, want 20", n)
	}
	env.importIssue(t, 1)
	if _, _, err := env.svc.ImportIssue(ctx, ws, IssueRef{Owner: "acme", Repo: "gadgets", Number: 5}, seeAllProjects); err != nil {
		t.Fatalf("import gadgets#5: %v", err)
	}

	cycle := func(wantRepos int) map[int64]int {
		t.Helper()
		from := len(env.api.seen())
		*env.now = env.now.Add(pollInterval)
		stats := env.poll(t)
		if stats.Repos != wantRepos || stats.Failed != 0 {
			t.Fatalf("cycle stats = %+v, want %d repositories", stats, wantRepos)
		}
		return env.requestsByRepo(from)
	}

	got := cycle(2)
	if got[pollRepoID] == 0 || got[gadgets.id] == 0 || len(got) != 2 {
		t.Fatalf("requests by repository = %v, want only %d and %d", got, pollRepoID, gadgets.id)
	}
	if n := env.count(t, `SELECT count(*) FROM github_repo_sync WHERE workspace_id = $1`, env.ws); n != 2 {
		t.Fatalf("pinned repositories = %d, want 2 (nothing pinned or started for the other 18)", n)
	}

	// Importing an issue of a third repository follows it from the next
	// cycle; the others stay quiet.
	if _, _, err := env.svc.ImportIssue(ctx, ws, IssueRef{Owner: "acme", Repo: "third", Number: 9}, seeAllProjects); err != nil {
		t.Fatalf("import third#9: %v", err)
	}
	got = cycle(3)
	if got[pollRepoID] == 0 || got[gadgets.id] == 0 || got[third.id] == 0 || len(got) != 3 {
		t.Fatalf("requests by repository after the import = %v", got)
	}

	// The last tracked issue of gadgets is found deleted: it drops out.
	env.f.Exec(t, `UPDATE issue_external SET state_reason = 'multica:deleted' WHERE workspace_id = $1 AND external_repo_id = $2`, env.ws, gadgets.id)
	if got = cycle(2); got[gadgets.id] != 0 {
		t.Fatalf("a repository with nothing tracked was still read: %v", got)
	}

	// A write from Multica still to settle keeps the repository followed:
	// the reconciler looks for it (here it is not found and handed over),
	// and then the repository drops out again.
	env.f.Exec(t, `INSERT INTO github_write_intent (workspace_id, op, repo_id, external_repo, payload_digest, actor_type, actor_id, state)
		VALUES ($1, 'create_issue', $2, 'acme/gadgets', ':', 'member', $3, 'unknown')`, env.ws, gadgets.id, env.owner)
	if got = cycle(3); got[gadgets.id] == 0 {
		t.Fatalf("a repository with a write to settle was not read: %v", got)
	}
	if n := env.count(t, `SELECT count(*) FROM github_write_intent WHERE workspace_id = $1 AND error LIKE 'needs_confirmation:%'`, env.ws); n != 1 {
		t.Fatal("the unanswered write was not handed over")
	}
	if got = cycle(2); got[gadgets.id] != 0 {
		t.Fatalf("a repository whose writes are all settled was still read: %v", got)
	}
}
