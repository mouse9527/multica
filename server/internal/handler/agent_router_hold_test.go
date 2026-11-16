// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
)

func TestRouterHoldsList(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM router_hold WHERE agent_id = ANY($1::uuid[])`, []string{f.agentA, f.agentB})
	})
	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusCreated).JSON(&sub)
	id := sub["id"].(string)
	dbfx.Exec(t, `
		INSERT INTO router_hold (workspace_id, subscription_id, agent_id, recipient_user_id, inbox_item_id, issue_id,
		                         item_type, severity, title, item, mode, status, decide_micros)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, 'mentioned', 'info', 'held item', '{}', 'shadow', 'shadow', 120)`,
		testWorkspaceID, id, f.agentA, testUserID, f.child)

	withRouterFlags(t, false)
	routerCall(t, newRequest("GET", "/api/router/holds", nil)).Want(http.StatusNotFound)

	withRouterFlags(t, true)
	type list struct {
		Holds []map[string]any `json:"holds"`
		Stats map[string]any   `json:"stats"`
	}
	var owner list
	routerCall(t, newRequest("GET", "/api/router/holds?subscription_id="+id, nil)).Want(http.StatusOK).JSON(&owner)
	if len(owner.Holds) != 1 || owner.Holds[0]["title"] != "held item" || owner.Holds[0]["in_inbox"] != false ||
		owner.Holds[0]["issue_identifier"] == "" || owner.Holds[0]["decide_micros"] != float64(120) {
		t.Fatalf("owner's holds = %v", owner.Holds)
	}
	if owner.Stats != nil {
		t.Fatalf("stats = %v, want null with no interceptor registered", owner.Stats)
	}

	// Another member does not see the owner's items; the subscribed agent
	// sees its subscription's, another agent does not.
	var other list
	routerCall(t, newRequestAs(f.member, "GET", "/api/router/holds", nil)).Want(http.StatusOK).JSON(&other)
	if len(other.Holds) != 0 {
		t.Fatalf("another member's holds = %v", other.Holds)
	}
	var agentA, agentB list
	routerCall(t, asAgent(newRequest("GET", "/api/router/holds", nil), f.agentA, f.taskA)).Want(http.StatusOK).JSON(&agentA)
	routerCall(t, asAgent(newRequest("GET", "/api/router/holds", nil), f.agentB, routerRun(t, f, f.agentB, "", "running"))).
		Want(http.StatusOK).JSON(&agentB)
	if len(agentA.Holds) != 1 || len(agentB.Holds) != 0 {
		t.Fatalf("agent A holds = %v, agent B holds = %v", agentA.Holds, agentB.Holds)
	}
	routerCall(t, newRequest("GET", "/api/router/holds?limit=0", nil)).Want(http.StatusBadRequest)
	routerCall(t, newRequest("GET", "/api/router/holds?subscription_id=nope", nil)).Want(http.StatusBadRequest)
}
