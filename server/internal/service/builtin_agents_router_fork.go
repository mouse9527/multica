// Added by the mouse9527/multica fork: Mika's pointer to the Super Agent guide (M-150).

package service

import (
	"context"
	"strings"

	"github.com/multica-ai/multica/server/internal/featureflags"
)

// mikaRouterGuidePointer is all Mika's prompt says about Super Agents. The
// guide itself ships in the CLI (`multica router guide`) and is read only when
// a request needs it, so ordinary runs do not pay for it.
const mikaRouterGuidePointer = "## Super Agent\n\n" +
	"When the member asks to make a chat a Super Agent (technical name: router), to change its scopes or settings, " +
	"to pause, resume or release it, or how it is doing — and whenever you run as a Super Agent — " +
	"run `multica router guide` first and follow it."

// WithMikaRouterGuide adds the Super Agent pointer to Mika's composed
// instructions while router_v1 is on, between the product-owned half and the
// workspace notes so the notes stay last. Any other agent, or router_v1 off,
// gets instructions back unchanged.
func (s *TaskService) WithMikaRouterGuide(ctx context.Context, agentSystemKey, displayName, instructions string) string {
	if agentSystemKey != MikaSystemKey || !featureflags.RouterV1Enabled(ctx, s.FeatureFlags) {
		return instructions
	}
	system := MikaSystemInstructions(displayName)
	rest, ok := strings.CutPrefix(instructions, system)
	if !ok {
		return instructions
	}
	return system + "\n\n" + mikaRouterGuidePointer + rest
}
