// Added by the mouse9527/multica fork: the Super Agent setup pointer in Mika's chats (M-150, moved to the chat layer by M-250).

package service

import (
	"context"

	"github.com/multica-ai/multica/server/internal/featureflags"
)

// mikaRouterSetupPointer tells Mika, in a chat no Super Agent is bound to,
// where the guide for setting one up is. It belongs to the chat layer, not to
// Mika's prompt (M-250): an agent's prompt says what the agent is for, and
// whatever concerns Super Agents comes from the chat a turn runs in. A turn in
// a bound chat gets the full Super Agent note instead, which points to the
// same guide. The guide itself ships in the CLI (`multica router guide`) and
// is read only when a request needs it, so ordinary turns do not pay for it.
const mikaRouterSetupPointer = "## Super Agent\n\n" +
	"When the member asks to make a chat a Super Agent (technical name: router), to change its scopes or settings, " +
	"to pause, resume or release it, or how it is doing, " +
	"run `multica router guide` first and follow it."

// SuperAgentSetupPointer is the chat-layer pointer for a turn of the given
// agent in a chat no Super Agent is bound to: the setup pointer for Mika while
// router_v1 is on, and "" for any other agent or with router_v1 off.
func (s *TaskService) SuperAgentSetupPointer(ctx context.Context, agentSystemKey string) string {
	if agentSystemKey != MikaSystemKey || !featureflags.RouterV1Enabled(ctx, s.FeatureFlags) {
		return ""
	}
	return mikaRouterSetupPointer
}
