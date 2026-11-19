// Added by the mouse9527/multica fork: resilient repo cache rebuilds (M-161).

package daemon

import (
	"context"
	"time"
)

// repoCacheBuildWaitBudget is how long a checkout request waits for a missing
// cache. `multica repo checkout` gives the whole request 5 minutes; answering
// before that lets the agent see why instead of a client timeout, and the
// build keeps running for the retry.
var repoCacheBuildWaitBudget = 4 * time.Minute

type repoCacheBuilder interface {
	BuildContext(ctx context.Context, workspaceID, repoURL string) error
}

// buildRepoCacheContext builds the cache for repoURL on a checkout's cache
// miss. Backends without cache-owned builds fall back to syncing the
// workspace's repos inside the request.
func (d *Daemon) buildRepoCacheContext(ctx context.Context, workspaceID, repoURL string, repos []RepoData) error {
	builder, ok := d.repoCache.(repoCacheBuilder)
	if !ok {
		return d.syncWorkspaceReposContext(ctx, workspaceID, repos)
	}
	waitCtx, cancel := context.WithTimeout(ctx, repoCacheBuildWaitBudget)
	defer cancel()
	err := builder.BuildContext(waitCtx, workspaceID, repoURL)
	if err != nil && ctx.Err() == nil {
		d.logger.Warn("repo cache build not ready", "workspace_id", workspaceID, "url", repoURL, "error", err)
	}
	return err
}
