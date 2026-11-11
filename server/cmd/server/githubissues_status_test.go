// Added by the mouse9527/multica fork: GitHub status changes reach the issue
// write path through the router's wiring.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// statusTestGitHub serves one repository with one issue whose state a test
// changes. The lists ignore "since": re-reading is harmless.
type statusTestGitHub struct {
	mu    sync.Mutex
	issue map[string]any
}

func (g *statusTestGitHub) set(state, reason string, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.issue = map[string]any{
		"id": 1001, "node_id": "I_1", "number": 1, "title": "Crash on start", "body": "",
		"state": state, "state_reason": reason, "labels": []any{},
		"updated_at": updated.UTC().Format(time.RFC3339), "user": map[string]any{"login": "stranger"},
	}
}

func (g *statusTestGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/user":
		w.Header().Set("X-OAuth-Scopes", "repo")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	case "/repos/acme/widgets":
		_, _ = w.Write([]byte(`{"id":42}`))
	case "/repositories/42/issues":
		_ = json.NewEncoder(w).Encode([]any{g.issue})
	case "/repositories/42/issues/comments":
		_, _ = w.Write([]byte(`[]`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// An issue closed on GitHub by someone outside Multica is done in Multica
// after the next poll: the router hands the poller the issue write path.
func TestGitHubCloseReachesDoneThroughRouterWiring(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	// The poller reads registered repositories across workspaces.
	pool := testutil.IsolatedDB(t, dsn)
	f := testutil.New(pool, "", "")
	suffix := uuid.NewString()[:8]
	owner := f.User(t, "Owner", "status-owner-"+suffix+"@example.invalid")
	ws := f.Workspace(t, "Status", "status-"+suffix)
	f.WorkspaceID, f.UserID = ws, owner
	f.Member(t, ws, owner, "owner")
	project := f.Project(t, "Widgets")
	f.Insert(t, "project_resource", testutil.Cols{
		"project_id": project, "workspace_id": ws,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/widgets"}`,
	})

	github := &statusTestGitHub{}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	github.set("open", "", t0)
	server := httptest.NewServer(github)
	t.Cleanup(server.Close)

	env := map[string]string{
		githubissues.EnvEnabled:        "true",
		githubissues.EnvToken:          routeTestToken,
		githubissues.EnvTokenExpiresAt: "never",
	}
	svc := githubissues.New(githubissues.LoadConfig(func(k string) string { return env[k] }),
		githubissues.Options{DB: pool, APIBase: server.URL})
	bus := events.New()
	registerActivityListeners(bus, db.New(pool))
	NewRouterWithOptions(pool, realtime.NewHub(), bus, analytics.NoopClient{}, nil, RouterOptions{GitHubIssues: svc})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.Start(ctx)
	for deadline := time.Now().Add(5 * time.Second); !svc.Usable(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("token was not validated against the fake GitHub")
		}
	}

	status := func() string {
		var s string
		f.QueryRow(t, `SELECT i.status FROM issue i JOIN issue_external x ON x.issue_id = i.id
			WHERE x.workspace_id = $1 AND x.external_number = 1`, ws).Scan(&s)
		return s
	}
	poll := func() {
		t.Helper()
		stats, err := svc.PollOnce(ctx)
		if err != nil || stats.Failed != 0 {
			t.Fatalf("poll: %+v %v", stats, err)
		}
	}
	poll()
	if got := status(); got != "backlog" {
		t.Fatalf("imported status = %q, want backlog", got)
	}

	github.set("closed", "completed", t0.Add(time.Minute))
	poll()
	if got := status(); got != "done" {
		t.Fatalf("status after the issue was closed on GitHub = %q, want done", got)
	}
	// The change is recorded like any other status change, as the sync
	// account (the workspace owner by default).
	if n := f.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1 AND actor_id = $2
		AND action = 'status_changed'`, ws, owner); n != 1 {
		t.Fatalf("status_changed activities by the owner = %d, want 1", n)
	}
}
