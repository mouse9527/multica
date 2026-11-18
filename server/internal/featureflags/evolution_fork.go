// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// EvolutionV1 gates the fork's built-in Evolution agent (FF_EVOLUTION_V1):
// the /api/evolution routes and the settings entry. Off by default; off means
// upstream behavior. It lives in its own file so keys.go only carries the one
// line that publishes it to the web client.
const EvolutionV1 = "evolution_v1"

func EvolutionV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, EvolutionV1, false)
}
