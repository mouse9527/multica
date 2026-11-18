// Added by the mouse9527/multica fork: live Super Agent settings (M-147).

package handler

import (
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
)

type routerChangeSeen struct {
	workspaceID, subscriptionID, action, actorType, actorID string
}

// watchRouterChanges records every router:subscription_changed event; the
// bus is shared, so callers keep only their own subscription's.
func watchRouterChanges(t *testing.T) func(subscriptionID string) []routerChangeSeen {
	t.Helper()
	var mu sync.Mutex
	var seen []routerChangeSeen
	testHandler.Bus.Subscribe(routerSubscriptionChangedEvent, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		id, _ := p["subscription_id"].(string)
		action, _ := p["action"].(string)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, routerChangeSeen{e.WorkspaceID, id, action, e.ActorType, e.ActorID})
	})
	return func(subscriptionID string) []routerChangeSeen {
		mu.Lock()
		defer mu.Unlock()
		var out []routerChangeSeen
		for _, s := range seen {
			if s.subscriptionID == subscriptionID {
				out = append(out, s)
			}
		}
		return out
	}
}

func TestRouterSubscriptionWritesAnnounceThemselves(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	seen := watchRouterChanges(t)

	// The agent subscribes itself from its run, as the Super Agent does when
	// asked in chat; the member then changes it from the CLI or the web.
	var sub map[string]any
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", map[string]any{
		"scope_type": "issue_tree", "scope_id": f.root,
	}), f.agentA, f.taskA)).Want(http.StatusCreated).JSON(&sub)
	id, _ := sub["id"].(string)
	routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"debounce_seconds": 30})).Want(http.StatusOK)
	routerCall(t, newRequest("POST", "/api/router/subscriptions/"+id+"/pause", nil)).Want(http.StatusOK)
	routerCall(t, newRequest("POST", "/api/router/subscriptions/"+id+"/resume", nil)).Want(http.StatusOK)
	routerCall(t, newRequest("DELETE", "/api/router/subscriptions/"+id, nil)).Want(http.StatusNoContent)
	// A refused write announces nothing.
	routerCall(t, newRequest("DELETE", "/api/router/subscriptions/"+id, nil)).Want(http.StatusNotFound)

	member := func(action string) routerChangeSeen {
		return routerChangeSeen{testWorkspaceID, id, action, "member", testUserID}
	}
	want := []routerChangeSeen{
		{testWorkspaceID, id, routerChangeCreated, "agent", f.agentA},
		member(routerChangeUpdated),
		member(routerChangePaused),
		member(routerChangeResumed),
		member(routerChangeDeleted),
	}
	if got := seen(id); !slices.Equal(got, want) {
		t.Fatalf("announced %+v\nwant %+v", got, want)
	}
}
