// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

// Package evolution holds the fork's Evolution feature: a built-in system
// agent that proposes improvements to a workspace's agents, workflows, and
// team for an owner to approve. P1 (this slice) is the agent's identity and
// its one-click enable; it reads no workspace data and registers no jobs.
package evolution

import (
	"context"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// FlagV1 is the feature flag (FF_EVOLUTION_V1), declared in a fork file of
// internal/featureflags so the web client can be told about it. Off by
// default; off means /api/evolution answers 404 and nothing else changes.
const FlagV1 = featureflags.EvolutionV1

// Enabled reports whether evolution_v1 is on. It is read per request.
func Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return featureflags.EvolutionV1Enabled(ctx, flags)
}
