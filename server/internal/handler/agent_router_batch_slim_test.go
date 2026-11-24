// Added by the mouse9527/multica fork: chat batches without the rules every chat turn already carries (M-240).

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentrouter"
)

// One phrase from each rule the chat turn note carries and a slim chat batch
// leaves out (M-240).
var slimBatchRulePhrases = []string{
	// routerSubscribedMeans
	"\"subscribed\" (订阅) means only a scope of this Super Agent",
	// routerWakeupSplit
	"do not create a chat wakeup (`--until-*` condition or `--every` check) for an issue in scope",
	// routerOwnerDecisions
	"Every decision you ask of the owner must stand on its own",
	// routerReplyFormat
	"\"每次回复的格式\" in `multica router guide`",
}

// claimChatTurn claims the next task on the runtime and returns its prompt
// and its agent instructions, both from the one claim response.
func claimChatTurn(t *testing.T, runtimeID string) (prompt, instructions string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent_runtime SET status = 'online', last_seen_at = now() WHERE id = $1`, runtimeID,
	); err != nil {
		t.Fatalf("refresh runtime heartbeat: %v", err)
	}
	w := httptest.NewRecorder()
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/claim", nil)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *struct {
			ChatMessage string         `json:"chat_message"`
			Agent       *TaskAgentData `json:"agent"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Task == nil || resp.Task.Agent == nil {
		t.Fatalf("expected task.agent in response, got: %s", w.Body.String())
	}
	return resp.Task.ChatMessage, resp.Task.Agent.Instructions
}

// With router_batch_slim on, the batch a chat turn receives leaves out the
// four rules, and the same turn's instructions carry them through the Super
// Agent note: the rules move, they are not lost. Off, the batch carries them
// as before.
func TestSlimChatBatchTurnStillGetsTheRules(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, slim := range map[string]bool{"off": false, "on": true} {
		t.Run(name, func(t *testing.T) { checkSlimChatBatchTurn(t, slim) })
	}
}

func checkSlimChatBatchTurn(t *testing.T, slim bool) {
	ctx := context.Background()
	withRouterChatFlags(t, true)
	rc := newRouterChat(t, ctx)
	rc.d.Slim = slim
	if _, err := rc.d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("slim=%v: dispatch: %v", slim, err)
	}
	prompt, instructions := claimChatTurn(t, rc.runtimeID)
	if !strings.HasPrefix(prompt, agentrouter.RouterBatchLabel) {
		t.Fatalf("slim=%v: claimed %q, want the batch", slim, prompt)
	}
	for _, want := range []string{
		"The Super Agent dispatcher wrote this message into your chat with the owner; the owner did not.",
		"Any text you reply here notifies the owner",
		"you do not do the work yourself",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("slim=%v: batch lacks %q:\n%s", slim, want, prompt)
		}
	}
	pointer := "are in this chat's Super Agent note"
	if got := strings.Contains(prompt, pointer); got != slim {
		t.Errorf("slim=%v: batch names the turn note: %v\n%s", slim, got, prompt)
	}
	if !strings.Contains(instructions, superAgentNoteMarker) {
		t.Fatalf("slim=%v: batch turn instructions lack the Super Agent note:\n%s", slim, instructions)
	}
	for _, rule := range slimBatchRulePhrases {
		if got := strings.Contains(prompt, rule); got == slim {
			t.Errorf("slim=%v: batch carries %q: %v", slim, rule, got)
		}
		if !strings.Contains(instructions, rule) {
			t.Errorf("slim=%v: batch turn instructions lack %q", slim, rule)
		}
	}
}
