package githubissues

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestRepoClientAgainstDatabase runs the allowlist and the pin on the real
// tables: project_resource is the allowlist, github_repo_sync holds the pin.
func TestRepoClientAgainstDatabase(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	f := testutil.New(pool, "", "")
	suffix := uuid.NewString()[:8]
	queries := db.New(pool)
	ctx := context.Background()

	ws := f.Workspace(t, "Repo client", "repo-client-"+suffix)
	f.WorkspaceID = ws
	project := f.Project(t, "Synced")
	resource := f.Insert(t, "project_resource", testutil.Cols{
		"project_id":    project,
		"workspace_id":  ws,
		"resource_type": "github_repo",
		"resource_ref":  `{"url":"https://github.com/acme/widgets.git"}`,
	})
	other := f.Workspace(t, "Other", "repo-client-other-"+suffix)
	f.WorkspaceID = other
	otherProject := f.Project(t, "Other")
	f.Insert(t, "project_resource", testutil.Cols{
		"project_id":    otherProject,
		"workspace_id":  other,
		"resource_type": "github_repo",
		"resource_ref":  `{"url":"https://github.com/acme/secret"}`,
	})

	api := newFakeRepoAPI(t)
	api.setName("acme/widgets", 42)
	api.setName("acme/secret", 77)
	svc := newTestService(t, api.server.URL, "2027-03-01")
	svc.store = queries
	svc.state.markAccepted("octocat", testNow)
	wsID := util.MustParseUUID(ws)

	// A repository registered only in another workspace is refused.
	if _, err := svc.RepoClient(ctx, wsID, "acme", "secret"); !errors.Is(err, ErrRepoNotRegistered) {
		t.Fatalf("other workspace's repository: err = %v", err)
	}
	if got := api.seen(); len(got) != 0 {
		t.Fatalf("refused repository reached GitHub: %v", got)
	}

	repo, err := svc.RepoClient(ctx, wsID, "acme", "widgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("registered repository: %v %v", repo, err)
	}
	pin, err := queries.GetGithubRepoSync(ctx, db.GetGithubRepoSyncParams{WorkspaceID: wsID, RepoID: 42})
	if err != nil || pin.Owner != "acme" || pin.Name != "widgets" {
		t.Fatalf("pin = %+v, err = %v", pin, err)
	}

	// Renamed to acme/gadgets, with a new repository taking the old name: the
	// registered old name still reaches 42.
	api.setName("acme/gadgets", 42)
	api.setName("acme/widgets", 99)
	repo, err = svc.RepoClient(ctx, wsID, "acme", "widgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("old name after rename: %v %v", repo, err)
	}
	if _, err := repo.Do(ctx, http.MethodGet, "/issues", nil); err != nil {
		t.Fatal(err)
	}

	// The Owner points the resource at the new name: same id, same row.
	if _, err := pool.Exec(ctx, `UPDATE project_resource SET resource_ref = '{"url":"https://github.com/acme/gadgets"}' WHERE id = $1`, resource); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RepoClient(ctx, wsID, "acme", "widgets"); !errors.Is(err, ErrRepoNotRegistered) {
		t.Fatalf("old name after the resource moved: err = %v", err)
	}
	repo, err = svc.RepoClient(ctx, wsID, "acme", "gadgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("new name: %v %v", repo, err)
	}
	pins, err := queries.ListGithubRepoSyncsByWorkspace(ctx, wsID)
	if err != nil || len(pins) != 1 || pins[0].RepoID != 42 || pins[0].Name != "gadgets" {
		t.Fatalf("pins = %+v, err = %v", pins, err)
	}

	// Removing the resource removes the repository from the allowlist even
	// though its pin row stays.
	if _, err := pool.Exec(ctx, `DELETE FROM project_resource WHERE id = $1`, resource); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RepoClient(ctx, wsID, "acme", "gadgets"); !errors.Is(err, ErrRepoNotRegistered) {
		t.Fatalf("removed resource: err = %v", err)
	}
}
