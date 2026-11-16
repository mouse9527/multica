// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// withRouterFlags is withFeatureFlag for both Router flags at once.
func withRouterFlags(t *testing.T, dispatch bool) {
	t.Helper()
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	provider.Set(featureflags.RouterDispatch, featureflag.Rule{Default: dispatch})
	orig := testHandler.FeatureFlags
	testHandler.FeatureFlags = featureflag.NewService(provider)
	t.Cleanup(func() { testHandler.FeatureFlags = orig })
}

// routerRun is a live run of agent started by originator ("" for none).
func routerRun(t *testing.T, f routerFixture, agent, originator, status string) string {
	t.Helper()
	var runtime string
	if err := testPool.QueryRow(t.Context(), `SELECT runtime_id::text FROM agent WHERE id = $1`, agent).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	cols := testutil.Cols{"issue_id": f.root, "status": status, "runtime_id": runtime}
	if originator != "" {
		cols["originator_user_id"] = originator
		cols["accountable_user_id"] = originator
	}
	return dbfx.Task(t, agent, cols)
}

func cleanupRouterS2(t *testing.T, f routerFixture) {
	t.Cleanup(func() {
		agents := []string{f.agentA, f.agentB}
		dbfx.Exec(t, `DELETE FROM router_subscription_change WHERE subscription_id IN (
			SELECT id FROM router_subscription WHERE agent_id = ANY($1::uuid[]))`, agents)
		dbfx.Exec(t, `DELETE FROM owner_notice WHERE sender_id = ANY($1::uuid[])`, agents)
		dbfx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1 AND type = 'router_notice'`, testWorkspaceID)
	})
}

func TestRouterDelegatedSubscription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	body := map[string]any{"agent_id": f.agentB, "scope_type": "issue_tree", "scope_id": f.root}

	// No human behind the run, a plain member behind it, or a finished run:
	// an agent still cannot subscribe another agent.
	for name, task := range map[string]string{
		"no originator":    routerRun(t, f, f.agentA, "", "running"),
		"plain member":     routerRun(t, f, f.agentA, f.member, "running"),
		"run has finished": routerRun(t, f, f.agentA, testUserID, "completed"),
	} {
		t.Run(name, func(t *testing.T) {
			routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", body), f.agentA, task)).Want(http.StatusForbidden)
		})
	}

	// A run started by the workspace owner may.
	ownerRun := routerRun(t, f, f.agentA, testUserID, "running")
	var sub map[string]any
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", body), f.agentA, ownerRun)).Want(http.StatusCreated).JSON(&sub)
	if sub["agent_id"] != f.agentB || sub["created_by_type"] != "member" || sub["created_by_id"] != testUserID ||
		sub["owner_user_id"] != testUserID || sub["via_agent_id"] != f.agentA || sub["via_task_id"] != ownerRun {
		t.Fatalf("delegated subscription = %v", sub)
	}
	id := sub["id"].(string)

	// The same run may change it; the plain member's run may not.
	routerCall(t, asAgent(newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"debounce_seconds": 30}),
		f.agentA, routerRun(t, f, f.agentA, f.member, "running"))).Want(http.StatusForbidden)
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions/"+id+"/pause", nil), f.agentA, ownerRun)).Want(http.StatusOK)

	var changes struct {
		Changes []map[string]any `json:"changes"`
	}
	routerCall(t, newRequest("GET", "/api/router/subscriptions/"+id+"/changes", nil)).Want(http.StatusOK).JSON(&changes)
	if len(changes.Changes) != 2 {
		t.Fatalf("changes = %v", changes.Changes)
	}
	for i, action := range []string{"create", "pause"} {
		c := changes.Changes[i]
		if c["action"] != action || c["actor_type"] != "agent" || c["actor_id"] != f.agentA ||
			c["authorized_by_user_id"] != testUserID || c["via_agent_id"] != f.agentA || c["via_task_id"] != ownerRun {
			t.Fatalf("change %d = %v", i, c)
		}
	}

	// Subscribing itself still needs no member.
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", map[string]any{
		"scope_type": "issue_tree", "scope_id": f.root,
	}), f.agentA, f.taskA)).Want(http.StatusCreated)
}

func TestRouterUpdateSubscription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusCreated).JSON(&sub)
	id := sub["id"].(string)
	if sub["debounce_seconds"] != float64(60) || sub["max_wait_seconds"] != float64(600) || sub["max_runs_per_hour"] != float64(6) {
		t.Fatalf("defaults = %v", sub)
	}

	routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{
		"debounce_seconds": 30, "max_runs_per_hour": 2, "anchor_issue_id": f.child, "event_types": []string{"comment_created"},
	})).Want(http.StatusOK).JSON(&sub)
	if sub["debounce_seconds"] != float64(30) || sub["max_runs_per_hour"] != float64(2) || sub["anchor_issue_id"] != f.child ||
		sub["max_wait_seconds"] != float64(600) {
		t.Fatalf("updated = %v", sub)
	}
	for name, body := range map[string]map[string]any{
		"debounce too short":      {"debounce_seconds": 1},
		"max wait below debounce": {"max_wait_seconds": 20},
		"too many runs":           {"max_runs_per_hour": 1000},
		"unknown event type":      {"event_types": []string{"nope"}},
		"empty anchor":            {"anchor_issue_id": ""},
	} {
		t.Run(name, func(t *testing.T) {
			routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, body)).Want(http.StatusBadRequest)
		})
	}
	routerCall(t, newRequestAs(f.member, "PATCH", "/api/router/subscriptions/"+id, map[string]any{"debounce_seconds": 40})).Want(http.StatusForbidden)
}

func TestRouterStateAndNotices(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusCreated).JSON(&sub)
	id := sub["id"].(string)
	statePath := "/api/router/subscriptions/" + id + "/state"

	withRouterFlags(t, false)
	routerCall(t, newRequest("GET", statePath, nil)).Want(http.StatusNotFound)
	routerCall(t, newRequest("POST", "/api/router/notices", map[string]any{"title": "x"})).Want(http.StatusNotFound)

	withRouterFlags(t, true)
	taskB := routerRun(t, f, f.agentB, "", "running")
	routerCall(t, asAgent(newRequest("PUT", statePath, map[string]any{"state": "x", "expected_revision": 0}), f.agentB, taskB)).Want(http.StatusForbidden)
	routerCall(t, newRequestAs(f.member, "PUT", statePath, map[string]any{"state": "x", "expected_revision": 0})).Want(http.StatusForbidden)
	routerCall(t, asAgent(newRequest("PUT", statePath, map[string]any{"state": "x"}), f.agentA, f.taskA)).Want(http.StatusBadRequest)
	routerCall(t, asAgent(newRequest("PUT", statePath, map[string]any{"state": "notes", "expected_revision": 0}), f.agentA, f.taskA)).Want(http.StatusOK)
	var st map[string]any
	routerCall(t, newRequestAs(f.member, "GET", statePath, nil)).Want(http.StatusOK).JSON(&st)
	if st["state"] != "notes" || st["revision"] != float64(1) {
		t.Fatalf("state = %v", st)
	}

	// Another agent cannot notify through A's subscription; A can, and the
	// notice goes to the subscription's owner.
	routerCall(t, asAgent(newRequest("POST", "/api/router/notices", map[string]any{
		"subscription_id": id, "title": "nope",
	}), f.agentB, taskB)).Want(http.StatusForbidden)
	var sent struct {
		Notice  map[string]any `json:"notice"`
		Created bool           `json:"created"`
	}
	routerCall(t, asAgent(newRequest("POST", "/api/router/notices", map[string]any{
		"subscription_id": id, "title": "Decide", "body": "A or B", "refs": []string{f.child}, "urgency": "high", "dedupe_key": "decide-" + id,
	}), f.agentA, f.taskA)).Want(http.StatusCreated).JSON(&sent)
	if !sent.Created || sent.Notice["recipient_user_id"] != testUserID || sent.Notice["source_task_id"] != f.taskA {
		t.Fatalf("notice = %v", sent)
	}
	routerCall(t, asAgent(newRequest("POST", "/api/router/notices", map[string]any{
		"subscription_id": id, "title": "Decide", "dedupe_key": "decide-" + id,
	}), f.agentA, f.taskA)).Want(http.StatusOK)
	// Another member reusing the key gets a notice of their own, not A's.
	var theirs struct {
		Notice  map[string]any `json:"notice"`
		Created bool           `json:"created"`
	}
	routerCall(t, newRequestAs(f.member, "POST", "/api/router/notices", map[string]any{
		"title": "Mine", "dedupe_key": "decide-" + id,
	})).Want(http.StatusCreated).JSON(&theirs)
	if !theirs.Created || theirs.Notice["id"] == sent.Notice["id"] || theirs.Notice["body"] != "" || theirs.Notice["recipient_user_id"] != f.member {
		t.Fatalf("member reusing the key = %v", theirs)
	}
	routerCall(t, asAgent(newRequest("POST", "/api/router/notices", map[string]any{"title": ""}), f.agentA, f.taskA)).Want(http.StatusBadRequest)
	routerCall(t, asAgent(newRequest("POST", "/api/router/notices", map[string]any{"title": "x", "urgency": "panic"}), f.agentA, f.taskA)).Want(http.StatusBadRequest)

	var mine struct {
		Notices []map[string]any `json:"notices"`
	}
	routerCall(t, newRequest("GET", "/api/router/notices?subscription_id="+id, nil)).Want(http.StatusOK).JSON(&mine)
	if len(mine.Notices) != 1 {
		t.Fatalf("owner's notices = %v", mine.Notices)
	}
}
