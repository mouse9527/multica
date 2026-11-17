// Added by the mouse9527/multica fork: Router marker in the chat list (M-108).

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type routerChatList struct {
	Chats []agentrouter.ChatSummary `json:"chats"`
}

// boundSubscription creates a subscription of agentID owned by owner and
// binds it to chat directly, as a binding made before the owner lost sight of
// the scope would be.
func boundSubscription(t *testing.T, agentID, owner, scopeType, scopeID, chat string) string {
	t.Helper()
	types, _ := agentrouter.NormalizeEventTypes(nil)
	sub, err := agentrouter.NewStore(testPool, testPool).Create(context.Background(), agentrouter.CreateParams{
		WorkspaceID: testWorkspaceID, AgentID: agentID, OwnerUserID: owner,
		ScopeType: scopeType, ScopeID: scopeID, EventTypes: types,
		CreatedByType: "member", CreatedByID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRouterChat(t, sub.ID)
	dbfx.Exec(t, `INSERT INTO router_chat_anchor (subscription_id, workspace_id, chat_session_id) VALUES ($1, $2, $3)`,
		sub.ID, testWorkspaceID, chat)
	return sub.ID
}

// TestRouterChatsSummarizeTheMembersRouters: one request lists the Routers
// among the caller's own chats with their scope names and pause reasons;
// other members' Routers and unbound subscriptions are left out.
func TestRouterChatsSummarizeTheMembersRouters(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	project := dbfx.Project(t, "router chat list project")
	chat := dbfx.ChatSession(t, f.agentA)
	plainChat := dbfx.ChatSession(t, f.agentA)
	othersChat := dbfx.ChatSession(t, f.agentB, testutil.Cols{"creator_id": f.member})

	tree := boundSubscription(t, f.agentA, testUserID, agentrouter.ScopeIssueTree, f.root, chat)
	proj := boundSubscription(t, f.agentA, testUserID, agentrouter.ScopeProject, project, chat)
	boundSubscription(t, f.agentB, f.member, agentrouter.ScopeIssueTree, f.root, othersChat)
	dbfx.Exec(t, `UPDATE router_subscription SET enabled = false, paused_reason = 'manual' WHERE id = $1`, proj)

	withRouterChatFlags(t, false)
	routerCall(t, newRequest("GET", "/api/router/chats", nil)).Want(http.StatusNotFound)

	withRouterChatFlags(t, true)
	var got routerChatList
	routerCall(t, newRequest("GET", "/api/router/chats", nil)).Want(http.StatusOK).JSON(&got)
	var mine *agentrouter.ChatSummary
	for i, c := range got.Chats {
		switch c.ChatSessionID {
		case chat:
			mine = &got.Chats[i]
		case plainChat, othersChat:
			t.Fatalf("chat %s listed as a Router: %+v", c.ChatSessionID, c)
		}
	}
	if mine == nil || len(mine.Scopes) != 2 {
		t.Fatalf("Router chat summary = %+v", mine)
	}
	if len(mine.PausedReasons) != 1 || mine.PausedReasons[0] != "manual" {
		t.Fatalf("paused reasons = %v, want [manual]", mine.PausedReasons)
	}
	byID := map[string]agentrouter.ChatSummaryScope{}
	for _, s := range mine.Scopes {
		byID[s.SubscriptionID] = s
	}
	if s := byID[tree]; !s.Visible || s.Name != "router root" || s.Identifier == "" || s.PausedReason != nil {
		t.Fatalf("issue tree scope = %+v", s)
	}
	if s := byID[proj]; !s.Visible || s.Name != "router chat list project" || s.Identifier != "" ||
		s.PausedReason == nil || *s.PausedReason != "manual" {
		t.Fatalf("project scope = %+v", s)
	}
}

// TestRouterChatsHideNamesOutsideTheMembersSight: a scope in a private
// project the member cannot see is listed without its name.
func TestRouterChatsHideNamesOutsideTheMembersSight(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	withRouterChatFlags(t, true)
	private := dbfx.Project(t, "router chat list private", testutil.Cols{"visibility": "private"})
	root := dbfx.Issue(t, "router chat list private root", testutil.Cols{"project_id": private})
	chat := dbfx.ChatSession(t, f.agentA, testutil.Cols{"creator_id": f.member})
	boundSubscription(t, f.agentA, f.member, agentrouter.ScopeIssueTree, root, chat)
	boundSubscription(t, f.agentA, f.member, agentrouter.ScopeProject, private, chat)

	var got routerChatList
	routerCall(t, newRequestAs(f.member, "GET", "/api/router/chats", nil)).Want(http.StatusOK).JSON(&got)
	if len(got.Chats) != 1 || got.Chats[0].ChatSessionID != chat || len(got.Chats[0].Scopes) != 2 {
		t.Fatalf("chats = %+v", got.Chats)
	}
	for _, s := range got.Chats[0].Scopes {
		if s.Visible || s.Name != "" || s.Identifier != "" {
			t.Fatalf("hidden scope leaked: %+v", s)
		}
	}

	// The same scopes are named once the project is open to the workspace.
	dbfx.Exec(t, `UPDATE project SET visibility = 'workspace' WHERE id = $1`, private)
	routerCall(t, newRequestAs(f.member, "GET", "/api/router/chats", nil)).Want(http.StatusOK).JSON(&got)
	for _, s := range got.Chats[0].Scopes {
		if !s.Visible || s.Name == "" {
			t.Fatalf("visible scope without a name: %+v", s)
		}
	}
}
