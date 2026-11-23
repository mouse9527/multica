// Added by the mouse9527/multica fork: quiet Super Agent batch replies (M-210).

package agentrouter

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Only a batch turn's quiet reply loses the alert: a turn the owner started
// alerts whatever its reply says, and so does a batch reply that asks
// something of the owner.
func TestChatReplyAlertDropsOnlyQuietBatchReplies(t *testing.T) {
	e := newEnv(t)
	chat := e.fx.ChatSession(t, e.agentA)
	turn := func(inputs ...string) string {
		t.Helper()
		task := e.fx.Task(t, e.agentA, testutil.Cols{
			"runtime_id": e.runtimeOf(e.agentA), "chat_session_id": chat, "status": "completed",
		})
		e.fx.Exec(t, "UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1", task)
		for _, kind := range inputs {
			e.fx.Insert(t, "chat_message", testutil.Cols{
				"chat_session_id": chat, "role": "user", "content": "input", "task_id": task, "message_kind": kind,
			})
		}
		return task
	}
	batchTurn := turn("router_batch")
	ownerTurn := turn("message")
	mixedTurn := turn("router_batch", "message")
	noInput := turn()

	const quiet = "[无需你处理]\nM-20 开发在推进。"
	for _, c := range []struct {
		name, task, content string
		alert               bool
	}{
		{"quiet batch reply", batchTurn, quiet, false},
		{"runtime error in a batch turn", batchTurn, "API Error: 504 Gateway Timeout", false},
		{"batch reply for the owner", batchTurn, "M-20 验收通过，请批准合并。", true},
		{"owner turn with the marker", ownerTurn, quiet, true},
		{"owner turn ending in a runtime error", ownerTurn, "API Error: 504", true},
		{"batch with an owner message", mixedTurn, quiet, true},
		{"turn without inputs", noInput, quiet, true},
		{"unknown input owner", "", quiet, true},
	} {
		if got := e.store.ChatReplyAlert(e.ctx, c.task, c.content); got != c.alert {
			t.Errorf("%s: alert = %v, want %v", c.name, got, c.alert)
		}
	}
}
