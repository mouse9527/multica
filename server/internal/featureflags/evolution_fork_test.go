// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// The web client shows the Evolution settings entry only while evolution_v1
// is on, so the flag must be published, and published as off by default.
func TestFrontendEvolutionFollowsTheFlag(t *testing.T) {
	for _, on := range []bool{true, false} {
		provider := featureflag.NewStaticProvider()
		provider.Set(EvolutionV1, featureflag.Rule{Default: on})
		got, ok := EvaluateFrontendPublicFlags(context.Background(), featureflag.NewService(provider))[EvolutionV1]
		if !ok || got != on {
			t.Errorf("evolution_v1=%v: published %v (present %v)", on, got, ok)
		}
	}
	if EvaluateFrontendPublicFlags(context.Background(), featureflag.NewService(featureflag.NewStaticProvider()))[EvolutionV1] {
		t.Error("evolution_v1 must default to off")
	}
}
