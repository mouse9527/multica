// Added by the mouse9527/multica fork: live Super Agent settings (M-147).
//
// The web client caches the subscription list without expiry, so a change made
// through the CLI or by an agent never reached a settings view that had already
// read it. Every write through the subscription API now announces itself; the
// client refetches the Router queries on it. The payload carries ids only: the
// list itself is read back through the API, which applies its own checks.

package handler

// routerSubscriptionChangedEvent is broadcast to the workspace whenever a
// subscription is created, changed, paused, resumed or deleted through the API.
const routerSubscriptionChangedEvent = "router:subscription_changed"

const (
	routerChangeCreated = "created"
	routerChangeUpdated = "updated"
	routerChangePaused  = "paused"
	routerChangeResumed = "resumed"
	routerChangeDeleted = "deleted"
)

func (h *Handler) publishRouterSubscriptionChanged(workspaceID, subscriptionID, action, actorType, actorID string) {
	h.publish(routerSubscriptionChangedEvent, workspaceID, actorType, actorID, map[string]any{
		"subscription_id": subscriptionID,
		"action":          action,
	})
}
