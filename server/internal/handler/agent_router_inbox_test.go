// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/featureflags"
)

func TestRouterInboxResolveSetting(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - $2 WHERE id = $1`,
			testWorkspaceID, agentrouter.WorkspaceSettingInboxResolve)
	})

	withFeatureFlag(t, testHandler, featureflags.RouterV1, false)
	routerCall(t, newRequest("GET", "/api/router/inbox-resolve", nil)).Want(http.StatusNotFound)
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)

	type state struct {
		Enabled bool                            `json:"enabled"`
		Config  *agentrouter.InboxResolveConfig `json:"config"`
		Survey  *agentrouter.InboxSurvey        `json:"survey"`
	}
	var before state
	routerCall(t, newRequest("GET", "/api/router/inbox-resolve", nil)).Want(http.StatusOK).JSON(&before)
	if before.Config != nil || before.Survey != nil {
		t.Fatalf("before = %+v, want no setting", before)
	}

	// Only owners and admins, and only members, change it.
	body := map[string]any{"waiting_statuses": []string{"in_review"}}
	routerCall(t, newRequestAs(f.member, "PUT", "/api/router/inbox-resolve", body)).Want(http.StatusForbidden)
	routerCall(t, asAgent(newRequest("PUT", "/api/router/inbox-resolve", body), f.agentA, f.taskA)).Want(http.StatusForbidden)
	routerCall(t, newRequest("PUT", "/api/router/inbox-resolve",
		map[string]any{"waiting_statuses": []string{"no_such_status"}})).Want(http.StatusBadRequest)
	routerCall(t, newRequest("PUT", "/api/router/inbox-resolve",
		map[string]any{"owner_user_id": f.agentA, "waiting_statuses": []string{"in_review"}})).Want(http.StatusBadRequest)

	var set state
	routerCall(t, newRequest("PUT", "/api/router/inbox-resolve", body)).Want(http.StatusOK).JSON(&set)
	if set.Config == nil || set.Config.OwnerUserID != testUserID || len(set.Config.WaitingStatuses) != 1 ||
		set.Config.WaitingStatuses[0] != "in_review" || set.Survey == nil {
		t.Fatalf("after set = %+v", set)
	}

	var off state
	routerCall(t, newRequest("PUT", "/api/router/inbox-resolve", map[string]any{"off": true})).Want(http.StatusOK).JSON(&off)
	if off.Config != nil {
		t.Fatalf("after off = %+v", off)
	}
}
