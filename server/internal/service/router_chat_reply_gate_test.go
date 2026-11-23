// Added by the mouse9527/multica fork: quiet Super Agent batch replies (M-210).

package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The gate only takes the alert off a reply: chat:done, the message and its
// content still go out. Without a gate every reply alerts, as upstream.
func TestChatReplyAlertGate(t *testing.T) {
	for _, name := range []string{"no gate", "gate keeps", "gate drops"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := sharedTestPool(t)
			fx := testutil.New(pool, "", "")
			owner := fx.User(t, "Gate owner", "chat-reply-gate@example.test")
			workspace := fx.Workspace(t, "Chat gate", "chat-gate-"+owner[:8])
			fx.UserID, fx.WorkspaceID = owner, workspace
			fx.Member(t, workspace, owner, "member")
			runtime := fx.Runtime(t, "Gate runtime")
			agent := fx.Agent(t, "Gate agent", runtime)
			session := fx.ChatSession(t, agent, testutil.Cols{"title": "Super Agent"})
			taskID := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": session, "status": "completed"})
			fx.Exec(t, "UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1", taskID)
			fx.Insert(t, "chat_message", testutil.Cols{
				"chat_session_id": session, "role": "user", "content": "batch", "task_id": taskID,
				"message_kind": protocol.ChatMessageKindRouterBatch,
			})
			messageID := fx.Insert(t, "chat_message", testutil.Cols{
				"chat_session_id": session, "role": "assistant", "content": "[无需你处理]\nNothing new.", "task_id": taskID, "message_kind": "message",
			})
			queries := db.New(pool)
			task, err := queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			message, err := queries.GetChatMessage(ctx, util.MustParseUUID(messageID))
			if err != nil {
				t.Fatal(err)
			}

			var asked struct{ input, content string }
			t.Cleanup(func() { SetChatReplyAlertGate(nil) })
			switch name {
			case "no gate":
				SetChatReplyAlertGate(nil)
			default:
				keep := name == "gate keeps"
				SetChatReplyAlertGate(func(_ context.Context, inputTaskID, content string) bool {
					if inputTaskID != taskID {
						return true
					}
					asked.input, asked.content = inputTaskID, content
					return keep
				})
			}

			bus := events.New()
			var got events.Event
			bus.Subscribe(protocol.EventChatDone, func(e events.Event) { got = e })
			svc := &TaskService{Queries: queries, Bus: bus}
			svc.broadcastChatDone(ctx, task, &message, false)
			payload, ok := got.Payload.(protocol.ChatDonePayload)
			if !ok || payload.MessageID != messageID || payload.Content != message.Content {
				t.Fatalf("chat:done lost the reply: %+v", got)
			}
			if name != "no gate" && (asked.input != taskID || asked.content != message.Content) {
				t.Fatalf("gate asked with input %q content %q, want %q and the reply", asked.input, asked.content, taskID)
			}
			if wantAlert := name != "gate drops"; (payload.Notification != nil) != wantAlert {
				t.Fatalf("alert = %+v, want alert %v", payload.Notification, wantAlert)
			}
		})
	}
}
