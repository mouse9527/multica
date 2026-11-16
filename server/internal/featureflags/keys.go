package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

const (
	// BillingWorkspaceSubscriptions gates the workspace-scoped entitlement,
	// Stripe Checkout, seat reconcile, and Billing Portal proxy surface. It is
	// deliberately off by default so the main repository can ship before the
	// managed cloud enables its matching billing.subscriptions capability.
	BillingWorkspaceSubscriptions = "billing_workspace_subscriptions"
	// ComposioMCPApps gates the Composio app management UI and — together with
	// the MUL-3963 permission_mode / invocation_targets access model it depends
	// on — the aligned Private / Public-to picker in the agent create flow.
	// The access model exists to gate Composio sharing, so the two ship on the
	// same switch.
	ComposioMCPApps = "composio_mcp_apps"
	// PluginsV1 gates the user-facing Plugin catalog and lifecycle management
	// APIs while the first product slice is dogfooded. It deliberately does not
	// gate pinned Task/Run execution: disabling discovery and management must not
	// mutate an immutable execution manifest that is already in flight.
	PluginsV1 = "plugins_v1"
	// TriageV1 gates the Triage inbox (MUL-7189): creating issues into Triage,
	// triage runs, and the Triage page. It is a GLOBAL switch — per-workspace
	// targeting has no production wiring — which is enough because a
	// workspace with no triager configured and no Triage issues sees nothing
	// either way. Turning it off stops new intake and triage runs but leaves
	// existing Triage issues workable.
	TriageV1 = "triage_v1"
	// LocalSearchIndex gates the sync endpoints behind Web/Desktop local search
	// (MUL-7754). On by default. Turning it off (FF_LOCAL_SEARCH_INDEX=off)
	// makes those endpoints answer 404, so clients stop syncing and search
	// through the server again without a client release.
	LocalSearchIndex = "local_search_index"
	// RouterV1 gates the fork's Router capture (mouse9527/multica fork, M-85):
	// router subscriptions and their API, the events.Bus listener and the
	// reconcile job. Off by default; off means upstream behavior.
	RouterV1 = "router_v1"
	// RouterDispatch gates the fork's Router dispatch and outlet (M-86):
	// anchor runs for captured events, subscription state, and notify
	// owner. It only takes effect with RouterV1 on. Off by default.
	RouterDispatch = "router_dispatch"
	// RouterChat makes dispatch write a chat-bound subscription's batches
	// into its chat session instead of starting anchor runs (mouse9527/multica
	// fork, M-96). It only takes effect with RouterDispatch on. Off by
	// default; off means dispatch as with RouterDispatch alone.
	RouterChat = "router_chat"
	// RouterHoldShadow runs the Router hold in shadow mode (mouse9527/multica
	// fork, M-97): every inbox write is checked against chat-bound
	// subscriptions, the items the hold would take are recorded and listed in
	// the next chat batch, and every write still reaches the inbox unchanged.
	// It only takes effect with RouterChat on. Off by default; off means no
	// inbox interceptor is registered.
	RouterHoldShadow = "router_hold_shadow"
	// HandoffReplyWake makes a member comment on an issue in a handoff status
	// also wake the issue's agent assignee, thread replies included
	// (mouse9527/multica fork, M-89). Off by default; off means upstream
	// behavior.
	HandoffReplyWake = "handoff_reply_wake"
	// agentBuilderCompat is no longer a release flag. Keep publishing the key
	// as enabled so installed desktop clients that still gate the AI creation
	// entry on this config decision receive the permanently enabled behavior.
	agentBuilderCompat = "agents_agent_builder"
	// agentSkillTogglesCompat is no longer a release flag. Keep publishing the
	// key as enabled so installed v0.4.0 desktop clients, which still gate the
	// switch on this config decision, receive the permanently enabled behavior.
	agentSkillTogglesCompat = "agents_skill_toggles"
	// resourceLabelsCompat is no longer a release flag. Keep publishing the key
	// as enabled for installed desktop clients from v0.4.0 through at least
	// v0.4.15, every release shipped before this change. Unlike the skill-toggle
	// gate above, which was removed client-side in v0.4.1, the resource-label
	// gate remained in every such client and fails closed (default false) if
	// the key stops being published.
	resourceLabelsCompat = "settings_resource_labels"
	// projectWorkflowsCompat keeps workflow UI available in installed desktop
	// clients that still gate it on this config decision and default to false.
	projectWorkflowsCompat = "project_workflows_v1"
)

var frontendPublicFlags = []string{
	BillingWorkspaceSubscriptions,
	ComposioMCPApps,
	PluginsV1,
}

func BillingWorkspaceSubscriptionsEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, BillingWorkspaceSubscriptions, false)
}

func ComposioMCPAppsEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, ComposioMCPApps, false)
}

func PluginsV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, PluginsV1, false)
}

func TriageV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, TriageV1, false)
}

func LocalSearchIndexEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, LocalSearchIndex, true)
}

// RouterV1Enabled is added by the mouse9527/multica fork (M-85).
func RouterV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, RouterV1, false)
}

// HandoffReplyWakeEnabled is added by the mouse9527/multica fork (M-89).
func HandoffReplyWakeEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, HandoffReplyWake, false)
}

// RouterDispatchEnabled is added by the mouse9527/multica fork (M-86). It
// requires RouterV1: dispatch consumes what capture writes.
func RouterDispatchEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return RouterV1Enabled(ctx, flags) && flags.IsEnabled(ctx, RouterDispatch, false)
}

// RouterChatEnabled is added by the mouse9527/multica fork (M-96). It requires
// RouterDispatch: the chat is where dispatch delivers.
func RouterChatEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return RouterDispatchEnabled(ctx, flags) && flags.IsEnabled(ctx, RouterChat, false)
}

// RouterHoldShadowEnabled is added by the mouse9527/multica fork (M-97). It
// requires RouterChat: the hold routes through a subscription's chat.
func RouterHoldShadowEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return RouterChatEnabled(ctx, flags) && flags.IsEnabled(ctx, RouterHoldShadow, false)
}

func EvaluateFrontendPublicFlags(ctx context.Context, flags *featureflag.Service) map[string]bool {
	out := make(map[string]bool, len(frontendPublicFlags)+4)
	for _, key := range frontendPublicFlags {
		out[key] = flags.IsEnabled(ctx, key, false)
	}
	out[agentBuilderCompat] = true
	out[agentSkillTogglesCompat] = true
	out[resourceLabelsCompat] = true
	out[projectWorkflowsCompat] = true
	return out
}
