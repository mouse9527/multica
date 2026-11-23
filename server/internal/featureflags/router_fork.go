// Added by the mouse9527/multica fork: the Router flags (M-85, M-86, M-89,
// M-96, M-97). They live in their own file so keys.go only carries the line
// that publishes router_chat to the web client; upstream adds its own flags
// at the end of keys.go's const block and function list, exactly where these
// used to sit (M-141).

package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

const (
	// RouterV1 gates the fork's Router capture (M-85): router subscriptions
	// and their API, the events.Bus listener and the reconcile job. Off by
	// default; off means upstream behavior.
	RouterV1 = "router_v1"
	// RouterDispatch gates the fork's Router dispatch and outlet (M-86):
	// anchor runs for captured events, subscription state, and notify
	// owner. It only takes effect with RouterV1 on. Off by default.
	RouterDispatch = "router_dispatch"
	// RouterChat makes dispatch write a chat-bound subscription's batches
	// into its chat session instead of starting anchor runs (M-96). It only
	// takes effect with RouterDispatch on. Off by default; off means
	// dispatch as with RouterDispatch alone.
	RouterChat = "router_chat"
	// RouterHoldShadow runs the Router hold in shadow mode (M-97): every
	// inbox write is checked against chat-bound subscriptions, the items the
	// hold would take are recorded and listed in the next chat batch, and
	// every write still reaches the inbox unchanged. It only takes effect
	// with RouterChat on. Off by default; off means no inbox interceptor is
	// registered.
	RouterHoldShadow = "router_hold_shadow"
	// HandoffReplyWake makes a member comment on an issue in a handoff status
	// also wake the issue's agent assignee, thread replies included (M-89).
	// Off by default; off means upstream behavior.
	HandoffReplyWake = "handoff_reply_wake"
)

func RouterV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, RouterV1, false)
}

func HandoffReplyWakeEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, HandoffReplyWake, false)
}

// RouterDispatchEnabled requires RouterV1: dispatch consumes what capture
// writes.
func RouterDispatchEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return RouterV1Enabled(ctx, flags) && flags.IsEnabled(ctx, RouterDispatch, false)
}

// RouterChatEnabled requires RouterDispatch: the chat is where dispatch
// delivers.
func RouterChatEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return RouterDispatchEnabled(ctx, flags) && flags.IsEnabled(ctx, RouterChat, false)
}

// RouterHoldShadowEnabled requires RouterChat: the hold routes through a
// subscription's chat.
func RouterHoldShadowEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return RouterChatEnabled(ctx, flags) && flags.IsEnabled(ctx, RouterHoldShadow, false)
}
