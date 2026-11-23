// Added by the mouse9527/multica fork: the private project fence on the
// /api/router routes that name an object by id (M-166). Upstream's
// TestEveryRouteByProjectObjectIDHidesPrivateProjects builds its router
// without feature flags, so every /api/router route answers 404 there; this
// is the same probe with router_v1 on. The registrations it backs are in
// server/cmd/server/fork_private_project_routes_test.go.

package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestRouterIssueCoveringHidesPrivateProjects: a workspace admin outside a
// private project gets for one of its issues exactly what a missing id gets;
// a project member gets the covering subscriptions.
func TestRouterIssueCoveringHidesPrivateProjects(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	fence := newRouterPrivateFence(t, f)

	answer := func(userID, issueID string) string {
		resp := routerCall(t, newRequestAs(userID, "GET", "/api/router/issues/"+issueID+"/covering", nil))
		return fmt.Sprintf("%d %s", resp.Code, strings.TrimSpace(resp.Text()))
	}
	const missing = "00000000-0000-0000-0000-000000000009"
	notFound := answer(fence.outsider, missing)
	hidden := strings.ReplaceAll(answer(fence.outsider, fence.hidden), fence.hidden, missing)
	if hidden != notFound {
		t.Fatalf("admin outside the project got %s; a missing id gets %s", hidden, notFound)
	}

	var out struct {
		Covering []map[string]any `json:"covering"`
	}
	routerCall(t, newRequestAs(fence.insider, "GET", "/api/router/issues/"+fence.hidden+"/covering", nil)).
		Want(http.StatusOK).JSON(&out)
	if len(out.Covering) != 1 || out.Covering[0]["subscription_id"] != fence.subscription {
		t.Fatalf("project member's covering = %+v, want subscription %s", out.Covering, fence.subscription)
	}
}

// TestRouterSubscriptionFiltersPrivateProjects: a subscription is a
// workspace object (its scope can span projects), so anyone in the workspace
// reads it; what it reaches inside a private project drops out for a caller
// outside that project instead.
func TestRouterSubscriptionFiltersPrivateProjects(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	fence := newRouterPrivateFence(t, f)

	routerCall(t, newRequestAs(fence.outsider, "GET", "/api/router/subscriptions/"+fence.subscription, nil)).
		Want(http.StatusOK)

	scope := func(userID string) map[string]bool {
		var out struct {
			Issues []struct {
				ID string `json:"id"`
			} `json:"issues"`
		}
		routerCall(t, newRequestAs(userID, "GET", "/api/router/subscriptions/"+fence.subscription+"/scope", nil)).
			Want(http.StatusOK).JSON(&out)
		ids := map[string]bool{}
		for _, issue := range out.Issues {
			ids[issue.ID] = true
		}
		return ids
	}
	if ids := scope(fence.outsider); ids[fence.hidden] || !ids[f.root] || !ids[f.child] {
		t.Fatalf("admin outside the project sees scope %v; want the root and public child, not %s", ids, fence.hidden)
	}
	if ids := scope(fence.insider); !ids[fence.hidden] {
		t.Fatalf("project member sees scope %v; want it to include %s", ids, fence.hidden)
	}
}

// routerPrivateFence is a subscription on the fixture's issue tree, one of
// whose children is in a private project: the insider (the test user, who
// owns the agents) is its member, the outsider is a workspace admin who is
// not, so only project membership tells them apart.
type routerPrivateFence struct {
	insider, outsider, hidden, subscription string
}

func newRouterPrivateFence(t *testing.T, f routerFixture) routerPrivateFence {
	t.Helper()
	fence := routerPrivateFence{insider: testUserID}
	suffix := fmt.Sprint(time.Now().UnixNano())
	fence.outsider = dbfx.User(t, "router private outsider", "router-private-"+suffix+"@example.test")
	dbfx.Member(t, testWorkspaceID, fence.outsider, "admin")

	private := dbfx.Project(t, "router private routes", testutil.Cols{"visibility": "private"})
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM project_member WHERE project_id = $1`, private) })
	dbfx.Exec(t, `INSERT INTO project_member (workspace_id, project_id, user_id, added_by) VALUES ($1, $2, $3, $3)`,
		testWorkspaceID, private, fence.insider)
	fence.hidden = dbfx.Issue(t, "router private routes child", testutil.Cols{"parent_issue_id": f.root, "project_id": private})

	var sub map[string]any
	routerCall(t, newRequestAs(fence.insider, "POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusCreated).JSON(&sub)
	fence.subscription, _ = sub["id"].(string)
	return fence
}
