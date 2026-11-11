package githubissues

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestSnapshotTransportAgainstDatabase runs the server-wide allowlist on the
// real project_resource table.
func TestSnapshotTransportAgainstDatabase(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	f := testutil.New(pool, "", "")
	suffix := uuid.NewString()[:8]

	for i, url := range []string{"https://github.com/acme/widgets.git", "git@github.com:acme/gadgets.git"} {
		f.WorkspaceID = f.Workspace(t, "Snapshot", "snapshot-"+suffix+"-"+string(rune('a'+i)))
		f.Insert(t, "project_resource", testutil.Cols{
			"project_id":    f.Project(t, "Synced"),
			"workspace_id":  f.WorkspaceID,
			"resource_type": "github_repo",
			"resource_ref":  `{"url":"` + url + `"}`,
		})
	}
	f.Insert(t, "project_resource", testutil.Cols{
		"project_id":    f.Project(t, "Docs"),
		"workspace_id":  f.WorkspaceID,
		"resource_type": "url",
		"resource_ref":  `{"url":"https://github.com/acme/handbook"}`,
	})

	svc := newTestService(t, "", "2027-03-01")
	svc.store = New(svc.cfg, Options{DB: pool, Logger: slog.New(slog.DiscardHandler)}).store
	svc.state.markAccepted("octocat", testNow)

	for _, tc := range []struct {
		repo string
		want error
	}{
		{"acme/widgets", nil},
		{"acme/gadgets", nil},
		{"acme/handbook", ErrRepoNotRegistered}, // not a github_repo resource
		{"acme/secret", ErrRepoNotRegistered},
	} {
		req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+tc.repo+"/pulls/1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.admitSnapshotRead(req); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.repo, err, tc.want)
		}
	}
}
