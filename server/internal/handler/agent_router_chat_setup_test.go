// Added by the mouse9527/multica fork: Router settings by instruction (M-105).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// chatRun is a live run of agentA in chat, started by originator ("" for a
// run without one, like a Router batch turn).
func chatRun(t *testing.T, f routerFixture, chat, originator string) string {
	t.Helper()
	var runtime string
	if err := testPool.QueryRow(t.Context(), `SELECT runtime_id::text FROM agent WHERE id = $1`, f.agentA).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	cols := testutil.Cols{"chat_session_id": chat, "status": "queued", "runtime_id": runtime}
	if originator != "" {
		cols["originator_user_id"] = originator
		cols["accountable_user_id"] = originator
	}
	task := dbfx.Task(t, f.agentA, cols)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id = $1`, task) })
	return task
}

func TestRouterCurrentChat(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	withRouterChatFlags(t, true)
	chat := dbfx.ChatSession(t, f.agentA, testutil.Cols{"title": "Feature lead"})
	run := chatRun(t, f, chat, testUserID)

	var got map[string]any
	routerCall(t, asAgent(newRequest("GET", "/api/router/chats/current", nil), f.agentA, run)).
		Want(http.StatusOK).JSON(&got)
	if got["chat_session_id"] != chat || got["title"] != "Feature lead" || got["creator_id"] != testUserID {
		t.Fatalf("current chat = %v", got)
	}

	finished := chatRun(t, f, chat, testUserID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, finished)
	for name, req := range map[string]*http.Request{
		"a member":            newRequest("GET", "/api/router/chats/current", nil),
		"an issue run":        asAgent(newRequest("GET", "/api/router/chats/current", nil), f.agentA, f.taskA),
		"a finished run":      asAgent(newRequest("GET", "/api/router/chats/current", nil), f.agentA, finished),
		"another agent's run": asAgent(newRequest("GET", "/api/router/chats/current", nil), f.agentB, run),
	} {
		t.Run(name, func(t *testing.T) { routerCall(t, req).Want(http.StatusBadRequest) })
	}

	withRouterChatFlags(t, false)
	routerCall(t, asAgent(newRequest("GET", "/api/router/chats/current", nil), f.agentA, run)).Want(http.StatusBadRequest)
}

// TestRouterAgentBindsChatForItsCreator: an agent told in a chat to make it
// a Router acts for the member who started that run, who must be the chat's
// creator; the Router is then that member's. Runs without that authority
// (another member's run, a Router batch turn) cannot bind the chat.
func TestRouterAgentBindsChatForItsCreator(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	withRouterChatFlags(t, true)
	chat := dbfx.ChatSession(t, f.agentA)
	ownerRun := chatRun(t, f, chat, testUserID)
	subscribe := func(task string) *testutil.Response {
		return routerCall(t, asAgent(newRequest("POST", "/api/router/subscriptions", map[string]any{
			"scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": chat,
		}), f.agentA, task))
	}

	for name, task := range map[string]string{
		"a run another member started": chatRun(t, f, chat, f.member),
		"a run without an originator":  chatRun(t, f, chat, ""),
	} {
		t.Run(name, func(t *testing.T) { subscribe(task).Want(http.StatusForbidden) })
	}

	var sub map[string]any
	subscribe(ownerRun).Want(http.StatusCreated).JSON(&sub)
	id := sub["id"].(string)
	cleanupRouterChat(t, id)
	if sub["anchor_chat_session_id"] != chat || sub["owner_user_id"] != testUserID ||
		sub["created_by_type"] != "member" || sub["created_by_id"] != testUserID || sub["via_agent_id"] != f.agentA {
		t.Fatalf("agent-created Router subscription = %v", sub)
	}

	// The same chat names the Router for list, and rebinding follows the rule.
	var list struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	routerCall(t, newRequest("GET", "/api/router/subscriptions?chat_session_id="+chat, nil)).Want(http.StatusOK).JSON(&list)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0]["id"] != id {
		t.Fatalf("subscriptions of the chat = %v", list.Subscriptions)
	}
	routerCall(t, asAgent(newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": "none"}), f.agentA, ownerRun)).
		Want(http.StatusOK)
	routerCall(t, newRequest("GET", "/api/router/subscriptions?chat_session_id="+chat, nil)).Want(http.StatusOK).JSON(&list)
	if len(list.Subscriptions) != 0 {
		t.Fatalf("unbound subscription still listed for the chat: %v", list.Subscriptions)
	}
	routerCall(t, asAgent(newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": chat}), f.agentA, chatRun(t, f, chat, ""))).
		Want(http.StatusForbidden)
	routerCall(t, asAgent(newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": chat}), f.agentA, ownerRun)).
		Want(http.StatusOK)
	routerCall(t, newRequest("GET", "/api/router/subscriptions?chat_session_id=not-a-uuid", nil)).Want(http.StatusBadRequest)
}
