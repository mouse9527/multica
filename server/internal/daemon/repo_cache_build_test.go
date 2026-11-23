// Added by the mouse9527/multica fork: resilient repo cache rebuilds (M-161).

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// pendingBuildCache models a cache build that outlasts the checkout request:
// BuildContext returns only when the caller stops waiting.
type pendingBuildCache struct {
	repoCacheBackend
	deadline    time.Time
	hadDeadline bool
}

func (c *pendingBuildCache) BuildContext(ctx context.Context, _, repoURL string) error {
	c.deadline, c.hadDeadline = ctx.Deadline()
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

// A request's shared wait deadline ends the build wait even when the build
// budget alone would wait longer.
func TestEnsureRepoReadyBuildWaitEndsAtTheRequestDeadline(t *testing.T) {
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

	shared := time.Now().Add(50 * time.Millisecond)
	_, err := d.ensureRepoReady(repocache.WithWaitDeadline(context.Background(), shared), "ws-1", repoURL)
	if !errors.Is(err, repocache.ErrCacheBuilding) {
		t.Fatalf("expected ErrCacheBuilding, got %v", err)
	}
	if !cache.hadDeadline || !cache.deadline.Equal(shared) {
		t.Fatalf("build wait deadline = %v (set %v), want the request's %v", cache.deadline, cache.hadDeadline, shared)
	}
}

// waitDeadlineRepoCache records the latest time a checkout may wait for
// cache-owned work.
type waitDeadlineRepoCache struct {
	recordingRepoCache
	waitDeadline time.Time
}

func (c *waitDeadlineRepoCache) CreateWorktreeContext(ctx context.Context, params repocache.WorktreeParams) (*repocache.WorktreeResult, error) {
	waitCtx, cancel := repocache.WaitContext(ctx, time.Hour)
	c.waitDeadline, _ = waitCtx.Deadline()
	cancel()
	return c.CreateWorktree(params)
}

// The checkout's file download wait shares the request's budget with the
// cache build wait, so the two together stay within it.
func TestRepoCheckoutSharesOneWaitBudget(t *testing.T) {
	const workspaceID = "ws-checkout"
	const repoURL = "https://github.com/org/repo.git"
	cache := &waitDeadlineRepoCache{recordingRepoCache: recordingRepoCache{lookupPath: "/cache/org/repo.git"}}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, workspaceID, repoURL, workDir, cache)

	start := time.Now()
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"url":"` + repoURL + `","workspace_id":"` + workspaceID + `","workdir":"` + workDir + `","task_id":"task-1"}`)
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	latest := time.Now().Add(repoCacheBuildWaitBudget)
	if cache.waitDeadline.Before(start) || cache.waitDeadline.After(latest) {
		t.Fatalf("checkout wait deadline = %v, want within %s of the request start %v", cache.waitDeadline, repoCacheBuildWaitBudget, start)
	}
}

// In-progress clones under .repos/.partial are cache bytes on disk, not
// cached repositories or a workspace's.
func TestScanDiskUsageCountsStagingClonesAsBytesOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsID := "11111111-1111-1111-1111-111111111111"
	writeFile(t, filepath.Join(root, ".repos", wsID, "widgets.git", "objects/pack/x"), 4000)
	writeFile(t, filepath.Join(root, ".repos", ".partial", wsID, "gadgets.git-123", "objects/pack/y"), 2000)

	report, err := ScanDiskUsage(root, nil)
	if err != nil {
		t.Fatalf("ScanDiskUsage: %v", err)
	}
	if report.RepoCacheSizeBytes != 6000 {
		t.Errorf("repo_cache_size_bytes = %d, want 6000", report.RepoCacheSizeBytes)
	}
	if report.RepoCacheCount != 1 {
		t.Errorf("repo_cache_count = %d, want 1 (a staging clone is not a cached repo)", report.RepoCacheCount)
	}
}
