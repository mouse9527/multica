// Added by the mouse9527/multica fork: one run per operation (M-280).

package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// RunDedupV1 gates the fork's deduplication of issue runs (FF_RUN_DEDUP_V1):
// a workflow handoff supersedes the same agent's run that was claimed moments
// earlier for the previous step, and a successful run folds the queued runs
// of the same agent whose every input it could read. Off by default; off
// means upstream behavior.
const RunDedupV1 = "run_dedup_v1"

func RunDedupV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, RunDedupV1, false)
}
