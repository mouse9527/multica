// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175), the reply format (M-206), wakeups on issues in scope (M-208), and the chat layer owning all Super Agent prompt content (M-250).

package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
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

// M-250: everything a prompt says about Super Agents comes from the chat
// layer. asSystemAgent turns a test chat's agent into a system agent after the
// message is sent, so only the claim sees the system key, and returns its name.
func asSystemAgent(t *testing.T, ctx context.Context, agentID, systemKey, notes string) string {
	t.Helper()
	var name string
	if err := testPool.QueryRow(ctx,
		`UPDATE agent SET system_key = $2, instructions = $3 WHERE id = $1 RETURNING name`,
		agentID, systemKey, notes).Scan(&name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE agent SET system_key = NULL WHERE id = $1`, agentID)
	})
	return name
}

// Evolution's rules that stop a Super Agent from reading and relaying issues.
var evolutionSuperAgentConflicts = []string{
	"Outside an analysis issue, do not scan",
	"Work only from what a member gives you directly",
	"When you are invoked for something outside improvement work",
}

// In a Super Agent chat, Evolution runs on its identity and notes plus the
// Super Agent note: its rules against reading issues and against anything but
// improvement work stay out.
func TestSuperAgentChatEvolutionRunsOnItsIdentity(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, true)
	rc := newRouterChat(t, ctx)
	sendDirectChat(t, ctx, rc.agentID, rc.sessionID, "anything new?")
	const notes = "Answer in Chinese."
	name := asSystemAgent(t, ctx, rc.agentID, service.EvolutionSystemKey, notes)

	got := claimAndDecodeAgent(t, rc.runtimeID).Instructions
	own, _ := service.SuperAgentTurnInstructions(service.EvolutionSystemKey, name, notes)
	if !strings.HasPrefix(got, own+"\n\n## Super Agent\n\n"+superAgentNoteMarker) {
		t.Fatalf("want Evolution's identity and notes, then the Super Agent note; got\n%s", got)
	}
	if !strings.Contains(got, "the workspace's Evolution agent") || !strings.HasSuffix(own, notes) {
		t.Errorf("identity or notes missing:\n%s", got)
	}
	for _, rule := range evolutionSuperAgentConflicts {
		if strings.Contains(got, rule) {
			t.Errorf("Super Agent turn carries Evolution's rule %q", rule)
		}
	}
}

// Evolution in a chat no Super Agent is bound to keeps its whole prompt.
func TestOrdinaryChatEvolutionPromptUnchanged(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, true)
	agentID, sessionID, runtimeID, _ := setupDirectChatSession(t, ctx, "ordinary evolution chat")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
	})
	sendDirectChat(t, ctx, agentID, sessionID, "hello")
	name := asSystemAgent(t, ctx, agentID, service.EvolutionSystemKey, "")
	if got, want := claimAndDecodeAgent(t, runtimeID).Instructions, service.ComposeEvolutionInstructions(name, ""); got != want {
		t.Fatalf("ordinary chat changed Evolution's prompt:\n%s", got)
	}
}

// Mika's prompt fits the Super Agent role, so a Super Agent turn keeps it
// whole and adds the note; the setup pointer stays out, the note covers it.
func TestSuperAgentChatMikaKeepsItsPrompt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, true)
	rc := newRouterChat(t, ctx)
	sendDirectChat(t, ctx, rc.agentID, rc.sessionID, "anything new?")
	name := asSystemAgent(t, ctx, rc.agentID, service.MikaSystemKey, "")

	got := claimAndDecodeAgent(t, rc.runtimeID).Instructions
	if !strings.HasPrefix(got, service.ComposeMikaInstructions(name, "")+"\n\n## Super Agent\n\n"+superAgentNoteMarker) {
		t.Fatalf("want Mika's prompt, then the Super Agent note; got\n%s", got)
	}
	if n := strings.Count(got, "## Super Agent"); n != 1 {
		t.Errorf("want one Super Agent section, got %d:\n%s", n, got)
	}
}

// Mika in a chat no Super Agent is bound to gets the setup pointer after its
// prompt, from the chat layer, and no Super Agent note.
func TestOrdinaryChatMikaGetsTheSetupPointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withRouterChatFlags(t, true)
	agentID, sessionID, runtimeID, _ := setupDirectChatSession(t, ctx, "ordinary mika chat")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
	})
	sendDirectChat(t, ctx, agentID, sessionID, "make this chat a Super Agent")
	name := asSystemAgent(t, ctx, agentID, service.MikaSystemKey, "")
	got := claimAndDecodeAgent(t, runtimeID).Instructions
	want := service.ComposeMikaInstructions(name, "") + "\n\n" +
		testHandler.TaskService.SuperAgentSetupPointer(ctx, service.MikaSystemKey)
	if got != want || !strings.Contains(got, "`multica router guide`") {
		t.Fatalf("want Mika's prompt and the setup pointer; got\n%s", got)
	}
	if strings.Contains(got, superAgentNoteMarker) {
		t.Errorf("ordinary chat carries the Super Agent note:\n%s", got)
	}
}

// Issue tasks never get the chat layer: Mika's issue prompt has no Super Agent
// section, and Evolution's analysis prompt is exactly what it was.
func TestSuperAgentChatLayerSkipsIssueTasks(t *testing.T) {
	ctx := context.Background()
	provider := featureflag.NewStaticProvider()
	for _, f := range []string{featureflags.RouterV1, featureflags.RouterDispatch, featureflags.RouterChat} {
		provider.Set(f, featureflag.Rule{Default: true})
	}
	flags := featureflag.NewService(provider)
	h := &Handler{FeatureFlags: flags, TaskService: &service.TaskService{FeatureFlags: flags}}
	task := &db.AgentTaskQueue{} // no chat session: an issue task

	for key, composed := range map[string]string{
		service.MikaSystemKey:      service.ComposeMikaInstructions("Mika", "notes"),
		service.EvolutionSystemKey: service.ComposeEvolutionInstructions("Evolution", "notes"),
	} {
		agent := db.Agent{Name: key, Instructions: "notes"}
		agent.SystemKey.String, agent.SystemKey.Valid = key, true
		got := h.withSuperAgentChatLayer(ctx, task, agent, composed)
		if got != composed {
			t.Errorf("%s: issue task prompt changed:\n%s", key, got)
		}
		if strings.Contains(got, "Super Agent") {
			t.Errorf("%s: issue task prompt mentions Super Agents", key)
		}
	}
}
