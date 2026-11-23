// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175), the reply format (M-206), and wakeups on issues in scope (M-208).

package handler

import (
	"context"
	"strings"
	"testing"
)

const superAgentNoteMarker = "This chat is a Super Agent"

// M-175: the owner reported a bug in a Super Agent chat, and the turn that
// answered never learned the chat was a Super Agent: the boundary rode only
// in batches. A turn the owner starts now carries the note, with its scopes
// and what "subscribed" means.
func TestSuperAgentChatOwnerTurnGetsTheNote(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, true)
	rc := newRouterChat(t, ctx)
	var subID, root string
	if err := testPool.QueryRow(ctx, `
		SELECT s.id::text, w.issue_prefix || '-' || i.number
		FROM router_chat_anchor ca
		JOIN router_subscription s ON s.id = ca.subscription_id
		JOIN issue i ON i.id = s.scope_id JOIN workspace w ON w.id = i.workspace_id
		WHERE ca.chat_session_id = $1`, rc.sessionID).Scan(&subID, &root); err != nil {
		t.Fatal(err)
	}

	sendDirectChat(t, ctx, rc.agentID, rc.sessionID, "B-1 is broken, please have someone fix it")
	got := claimAndDecodeAgent(t, rc.runtimeID).Instructions
	for _, want := range []string{
		superAgentNoteMarker,
		"- issue tree " + root + " (subscription " + subID + ", running)",
		"\"subscribed\" (订阅) means only a scope of this Super Agent",
		"`multica router scope <subscription-id>`",
		"the automatic `creator` one",
		"multica router subscribe --issue <id> --anchor-chat current",
		"`multica router guide`",
		// M-206: the reply format reaches every workspace's owner turns.
		"(「需要你处理」 in Chinese)",
		"Merged is not fixed",
		"\"每次回复的格式\" in `multica router guide`",
		// M-208: wakeup turns are chat turns, so the wakeup rules ride here too.
		"In a turn a wakeup starts, read the issue's current status",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("owner turn instructions lack %q:\n%s", want, got)
		}
	}
}

// With router_chat off the chat behaves as upstream, even though a
// subscription is still bound to it.
func TestSuperAgentChatNoteNeedsRouterChat(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, false)
	rc := newRouterChat(t, ctx)
	sendDirectChat(t, ctx, rc.agentID, rc.sessionID, "anything new?")
	got := claimAndDecodeAgent(t, rc.runtimeID).Instructions
	if strings.Contains(got, superAgentNoteMarker) || strings.Contains(got, "每次回复的格式") {
		t.Fatalf("router_chat off, instructions still carry the note:\n%s", got)
	}
}

// A chat no subscription is bound to gets nothing, router_chat on or not.
func TestSuperAgentChatNoteSkipsOrdinaryChats(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, true)
	agentID, sessionID, runtimeID, _ := setupDirectChatSession(t, ctx, "ordinary chat")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
	})
	sendDirectChat(t, ctx, agentID, sessionID, "hello")
	if got := claimAndDecodeAgent(t, runtimeID).Instructions; strings.Contains(got, superAgentNoteMarker) {
		t.Fatalf("ordinary chat instructions carry the note:\n%s", got)
	}
}
