// Added by the mouse9527/multica fork: which Super Agents cover an issue, the
// scope check on issues a Super Agent chat creates, and the overlap check on
// subscribe (M-194).

package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestRouterIssueCovering(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	withRouterChatFlags(t, true)
	chat := dbfx.ChatSession(t, f.agentA, testutil.Cols{"title": "covering chat"})
	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": chat,
	})).Want(http.StatusCreated).JSON(&sub)
	id := sub["id"].(string)
	cleanupRouterChat(t, id)

	var out struct {
		IssueID  string           `json:"issue_id"`
		Covering []map[string]any `json:"covering"`
	}
	routerCall(t, newRequest("GET", "/api/router/issues/"+f.child+"/covering", nil)).Want(http.StatusOK).JSON(&out)
	if out.IssueID != f.child || len(out.Covering) != 1 {
		t.Fatalf("covering = %+v", out)
	}
	c := out.Covering[0]
	if c["subscription_id"] != id || c["effective"] != true || c["scope_type"] != "issue_tree" ||
		c["chat_session_id"] != chat || c["chat_title"] != "covering chat" || c["agent_name"] != "router-handler-a" {
		t.Fatalf("covering[0] = %v", c)
	}

	// Another member sees the Super Agent, not the owner's chat.
	routerCall(t, newRequestAs(f.member, "GET", "/api/router/issues/"+f.child+"/covering", nil)).Want(http.StatusOK).JSON(&out)
	if c := out.Covering[0]; c["chat_bound"] != true || c["chat_session_id"] != nil || c["chat_title"] != nil {
		t.Fatalf("another member's view = %v", c)
	}

	routerCall(t, newRequest("GET", "/api/router/issues/00000000-0000-0000-0000-000000000009/covering", nil)).Want(http.StatusNotFound)
	withFeatureFlag(t, testHandler, featureflags.RouterV1, false)
	routerCall(t, newRequest("GET", "/api/router/issues/"+f.child+"/covering", nil)).Want(http.StatusNotFound)
}

func TestRouterSubscribeRefusesAnotherAgentsScope(t *testing.T) {
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
	aTree := sub["id"].(string)

	// Agent B on the subtree A already covers: refused, naming A.
	body := map[string]any{"agent_id": f.agentB, "scope_type": "issue_tree", "scope_id": f.child}
	var refused struct {
		Code     string           `json:"code"`
		Error    string           `json:"error"`
		Overlaps []map[string]any `json:"overlaps"`
	}
	routerCall(t, newRequest("POST", "/api/router/subscriptions", body)).Want(http.StatusConflict).JSON(&refused)
	if refused.Code != "router_scope_overlap" || len(refused.Overlaps) != 1 || refused.Overlaps[0]["subscription_id"] != aTree ||
		refused.Overlaps[0]["issue_count"] != float64(1) || !strings.Contains(refused.Error, "router-handler-a") {
		t.Fatalf("refusal = %+v", refused)
	}
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM router_subscription WHERE agent_id = $1`, f.agentB).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agent B subscriptions after the refusal = %d (%v), want 0", n, err)
	}

	// The same agent narrowing its own scope is not an overlap.
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.child,
	})).Want(http.StatusCreated)

	// Confirmed: allow_overlap subscribes anyway.
	body["allow_overlap"] = true
	routerCall(t, newRequest("POST", "/api/router/subscriptions", body)).Want(http.StatusCreated)
}

func TestIssueCreateInSuperAgentChatReportsScope(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	withRouterChatFlags(t, true)
	chat := dbfx.ChatSession(t, f.agentA)
	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": chat,
	})).Want(http.StatusCreated).JSON(&sub)
	cleanupRouterChat(t, sub["id"].(string))
	run := chatRun(t, f, chat, testUserID)

	created := 0
	create := func(task string, body map[string]any) map[string]any {
		t.Helper()
		// Distinct titles: an identical open title is refused as a duplicate.
		created++
		body["title"] = fmt.Sprintf("router created issue %d", created)
		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
		if task != "" {
			req = asAgent(req, f.agentA, task)
		}
		var issue map[string]any
		testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated).JSON(&issue)
		id := issue["id"].(string)
		t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue WHERE id = $1`, id) })
		return issue
	}
	scopeOf := func(issue map[string]any) map[string]any {
		s, _ := issue["super_agent_scope"].(map[string]any)
		return s
	}

	if s := scopeOf(create(run, map[string]any{"parent_issue_id": f.root})); s == nil || s["in_scope"] != true || s["hint"] != nil {
		t.Fatalf("sub-issue of the subscribed tree: super_agent_scope = %v", s)
	}
	out := create(run, map[string]any{})
	s := scopeOf(out)
	if s == nil || s["in_scope"] != false || s["chat_session_id"] != chat ||
		!strings.Contains(s["hint"].(string), "multica router subscribe --issue "+out["identifier"].(string)+" --anchor-chat current") {
		t.Fatalf("issue in no scope: super_agent_scope = %v", s)
	}

	// Not from a Super Agent chat run: a member, or the agent on an issue task.
	if s := scopeOf(create("", map[string]any{})); s != nil {
		t.Fatalf("member create carries super_agent_scope = %v", s)
	}
	if s := scopeOf(create(f.taskA, map[string]any{})); s != nil {
		t.Fatalf("issue-task create carries super_agent_scope = %v", s)
	}
	// router_chat off: the response is upstream's.
	withRouterChatFlags(t, false)
	if s := scopeOf(create(run, map[string]any{})); s != nil {
		t.Fatalf("router_chat off: super_agent_scope = %v", s)
	}
}
