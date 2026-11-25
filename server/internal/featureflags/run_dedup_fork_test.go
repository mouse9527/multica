// Added by the mouse9527/multica fork: one run per operation (M-280).

package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// Off means upstream behavior, so the flag must default to off and follow
// its provider when set.
func TestRunDedupFollowsTheFlag(t *testing.T) {
	ctx := context.Background()
	if RunDedupV1Enabled(ctx, nil) {
		t.Error("run_dedup_v1 must default to off without a flag service")
	}
	if RunDedupV1Enabled(ctx, featureflag.NewService(featureflag.NewStaticProvider())) {
		t.Error("run_dedup_v1 must default to off")
	}
	for _, on := range []bool{true, false} {
		provider := featureflag.NewStaticProvider()
		provider.Set(RunDedupV1, featureflag.Rule{Default: on})
		if got := RunDedupV1Enabled(ctx, featureflag.NewService(provider)); got != on {
			t.Errorf("run_dedup_v1=%v: got %v", on, got)
		}
	}
}
