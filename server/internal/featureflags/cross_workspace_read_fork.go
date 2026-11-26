// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// CrossWorkspaceRead gates the fork's cross-workspace read grants
// (FF_CROSS_WORKSPACE_READ): the grant routes, and accepting an agent's task
// token in a workspace that granted it read access. Off by default; off means
// upstream behavior: a task token that names another workspace is refused and
// the grant routes answer 404. There is no web UI, so the flag is not
// published to the client.
const CrossWorkspaceRead = "cross_workspace_read"

func CrossWorkspaceReadEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, CrossWorkspaceRead, false)
}
