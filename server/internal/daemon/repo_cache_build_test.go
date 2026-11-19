// Added by the mouse9527/multica fork: resilient repo cache rebuilds (M-161).

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// pendingBuildCache models a cache build that outlasts the checkout request:
// BuildContext returns only when the caller stops waiting.
type pendingBuildCache struct {
	repoCacheBackend
	hadDeadline bool
}

func (c *pendingBuildCache) BuildContext(ctx context.Context, _, repoURL string) error {
	_, c.hadDeadline = ctx.Deadline()
	<-ctx.Done()
	return &repocache.BuildPendingError{URL: repoURL, Running: time.Minute, Cause: context.Cause(ctx)}
}

// A checkout that misses the cache must answer before the CLI's own deadline
// with an error that says the build is still running, not hang until the
// client gives up (which used to kill the clone).
func TestEnsureRepoReadyStopsWaitingForSlowCacheBuild(t *testing.T) {
	orig := repoCacheBuildWaitBudget
	repoCacheBuildWaitBudget = 50 * time.Millisecond
	t.Cleanup(func() { repoCacheBuildWaitBudget = orig })

	const repoURL = "https://example.com/team/big.git"
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{{URL: repoURL}},
			ReposVersion: "v1",
		})
	})
	cache := &pendingBuildCache{repoCacheBackend: d.repoCache}
	d.repoCache = cache
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)

	_, err := d.ensureRepoReady(context.Background(), "ws-1", repoURL)
	if !errors.Is(err, repocache.ErrCacheBuilding) {
		t.Fatalf("expected ErrCacheBuilding, got %v", err)
	}
	if !strings.Contains(err.Error(), "still being built in the background") {
		t.Fatalf("error does not tell the agent to retry later: %v", err)
	}
	if !cache.hadDeadline {
		t.Fatal("the build wait must be bounded by the wait budget")
	}
}
