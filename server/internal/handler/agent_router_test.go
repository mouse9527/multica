// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// routerFixture is two agents owned by the test user, an issue tree, and a
// plain member who owns neither agent.
type routerFixture struct {
	agentA, agentB, root, child, member, taskA string
}

func newRouterFixture(t *testing.T) routerFixture {
	t.Helper()
	runtime := dbfx.Runtime(t, "router-handler-runtime")
	f := routerFixture{
		agentA: dbfx.Agent(t, "router-handler-a", runtime),
		agentB: dbfx.Agent(t, "router-handler-b", runtime),
		root:   dbfx.Issue(t, "router root"),
	}
	f.child = dbfx.Issue(t, "router child", testutil.Cols{"parent_issue_id": f.root})
	suffix := fmt.Sprint(time.Now().UnixNano())
	f.member = dbfx.User(t, "router plain member", "router-member-"+suffix+"@example.test")
	dbfx.Member(t, testWorkspaceID, f.member, "member")
	f.taskA = dbfx.Task(t, f.agentA, testutil.Cols{"issue_id": f.root, "status": "running", "runtime_id": runtime})
	t.Cleanup(func() {
		agents := []string{f.agentA, f.agentB}
		dbfx.Exec(t, `DELETE FROM router_event WHERE agent_id = ANY($1::uuid[])`, agents)
		dbfx.Exec(t, `DELETE FROM router_issue_seen WHERE subscription_id IN (
			SELECT id FROM router_subscription WHERE agent_id = ANY($1::uuid[]))`, agents)
		dbfx.Exec(t, `DELETE FROM router_subscription WHERE agent_id = ANY($1::uuid[])`, agents)
	})
	return f
}

// routerCall serves req through the mounted /api/router routes, so the flag
// gate and URL params are exercised as in production.
func routerCall(t *testing.T, req *http.Request) *testutil.Response {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/api/router", testHandler.MountAgentRouter)
	return testutil.Call(t, r.ServeHTTP, req)
}

func asAgent(req *http.Request, agentID, taskID string) *http.Request {
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	return req
}

func TestRouterAPIIsNotFoundWithFlagOff(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, false)
	routerCall(t, newRequest("GET", "/api/router/subscriptions", nil)).Want(http.StatusNotFound)
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": "00000000-0000-0000-0000-000000000001", "scope_type": "project",
		"scope_id": "00000000-0000-0000-0000-000000000002",
	})).Want(http.StatusNotFound)
	routerCall(t, newRequest("GET", "/api/router/events", nil)).Want(http.StatusNotFound)
}

func TestRouterSubscriptionLifecycle(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)

	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
		"event_types": []string{"comment_created", "status_changed"},
	})).Want(http.StatusCreated).JSON(&sub)
	id, _ := sub["id"].(string)
	if sub["scope_id"] != f.root || sub["anchor_issue_id"] != f.root || sub["enabled"] != true ||
		fmt.Sprint(sub["event_types"]) != "[status_changed comment_created]" || sub["capture_miss"] != float64(0) {
		t.Fatalf("created subscription = %v", sub)
	}

	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusConflict)

	var list struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	routerCall(t, newRequest("GET", "/api/router/subscriptions?agent_id="+f.agentA, nil)).Want(http.StatusOK).JSON(&list)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0]["id"] != id {
		t.Fatalf("list = %v", list.Subscriptions)
	}

	var scope struct {
		Issues []map[string]any `json:"issues"`
	}
	routerCall(t, newRequest("GET", "/api/router/subscriptions/"+id+"/scope", nil)).Want(http.StatusOK).JSON(&scope)
	if len(scope.Issues) != 2 {
		t.Fatalf("scope = %v, want root and child", scope.Issues)
	}

	var evs struct {
		Events []map[string]any `json:"events"`
	}
	routerCall(t, newRequest("GET", "/api/router/events?subscription_id="+id+"&state=pending", nil)).Want(http.StatusOK).JSON(&evs)
	if len(evs.Events) != 0 {
		t.Fatalf("events = %v", evs.Events)
	}
	routerCall(t, newRequest("GET", "/api/router/events?state=done", nil)).Want(http.StatusBadRequest)
	routerCall(t, newRequest("GET", "/api/router/events?since=yesterday", nil)).Want(http.StatusBadRequest)

	routerCall(t, newRequest("POST", "/api/router/subscriptions/"+id+"/pause", nil)).Want(http.StatusOK).JSON(&sub)
	if sub["enabled"] != false || sub["paused_reason"] != "manual" {
		t.Fatalf("paused = %v", sub)
	}
	routerCall(t, newRequest("POST", "/api/router/subscriptions/"+id+"/resume", nil)).Want(http.StatusOK).JSON(&sub)
	if sub["enabled"] != true || sub["paused_reason"] != nil {
		t.Fatalf("resumed = %v", sub)
	}
	routerCall(t, newRequest("DELETE", "/api/router/subscriptions/"+id, nil)).Want(http.StatusNoContent)
	routerCall(t, newRequest("GET", "/api/router/subscriptions/"+id, nil)).Want(http.StatusNotFound)
}

func TestRouterCreateValidation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)

	for name, body := range map[string]map[string]any{
		"member without agent_id": {"scope_type": "issue_tree", "scope_id": f.root},
		"unknown scope type":      {"agent_id": f.agentA, "scope_type": "workspace", "scope_id": f.root},
		"unknown event type":      {"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root, "event_types": []string{"issue_deleted"}},
		"project id not a uuid":   {"agent_id": f.agentA, "scope_type": "project", "scope_id": "not-a-uuid"},
	} {
		t.Run(name, func(t *testing.T) {
			routerCall(t, newRequest("POST", "/api/router/subscriptions", body)).Want(http.StatusBadRequest)
		})
	}
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": "00000000-0000-0000-0000-000000000009",
	})).Want(http.StatusNotFound)
}

func TestRouterPermissions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)

	// A member who does not own the agent and is not an admin cannot
	// subscribe it.
	routerCall(t, newRequestAs(f.member, "POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusForbidden)

	// An agent cannot subscribe another agent ...
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentB, "scope_type": "issue_tree", "scope_id": f.root,
	}), f.agentA, f.taskA)).Want(http.StatusForbidden)

	// ... but subscribes itself when agent_id is left out.
	var sub map[string]any
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", map[string]any{
		"scope_type": "issue_tree", "scope_id": f.root,
	}), f.agentA, f.taskA)).Want(http.StatusCreated).JSON(&sub)
	if sub["agent_id"] != f.agentA || sub["created_by_type"] != "agent" {
		t.Fatalf("agent self-subscription = %v", sub)
	}
	id, _ := sub["id"].(string)

	// The plain member can read it but not change it; the agent can.
	routerCall(t, newRequestAs(f.member, "GET", "/api/router/subscriptions/"+id, nil)).Want(http.StatusOK)
	routerCall(t, newRequestAs(f.member, "POST", "/api/router/subscriptions/"+id+"/pause", nil)).Want(http.StatusForbidden)
	routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions/"+id+"/pause", nil), f.agentA, f.taskA)).Want(http.StatusOK)
	// A workspace owner can change any subscription.
	routerCall(t, newRequest("DELETE", "/api/router/subscriptions/"+id, nil)).Want(http.StatusNoContent)
}

func TestRouterHidesEventsOfPrivateProjects(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RouterV1, true)
	f := newRouterFixture(t)
	private := dbfx.Project(t, "router private", testutil.Cols{"visibility": "private"})
	hidden := dbfx.Issue(t, "router hidden child", testutil.Cols{"parent_issue_id": f.root, "project_id": private})

	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusCreated).JSON(&sub)
	id, _ := sub["id"].(string)
	for _, issue := range []string{f.child, hidden} {
		dbfx.Exec(t, `
			INSERT INTO router_event (workspace_id, subscription_id, agent_id, event_key, kind, issue_id, source, occurred_at)
			VALUES ($1, $2, $3, 'test:' || $4::text, 'comment_created', $4::uuid, 'bus', now())`,
			testWorkspaceID, id, f.agentA, issue)
	}

	count := func(req *http.Request) int {
		var evs struct {
			Events []map[string]any `json:"events"`
		}
		routerCall(t, req).Want(http.StatusOK).JSON(&evs)
		return len(evs.Events)
	}
	// The workspace owner manages every project and sees both.
	if n := count(newRequest("GET", "/api/router/events?subscription_id="+id, nil)); n != 2 {
		t.Fatalf("owner sees %d events, want 2", n)
	}
	// A plain member outside the private project sees only the public one.
	if n := count(newRequestAs(f.member, "GET", "/api/router/events?subscription_id="+id, nil)); n != 1 {
		t.Fatalf("plain member sees %d events, want 1", n)
	}
}
