// Added by the mouse9527/multica fork: quiet Super Agent batch replies (M-210).

package service

import (
	"context"
	"sync/atomic"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ChatReplyAlertGate decides whether a finished chat turn's text reply raises
// the owner's chat reply alert (chat:reply_notification). inputTaskID is the
// task owning the turn's input, content the stored reply. It returns false to
// keep the reply in the chat without the alert; the reply, its unread state
// and chat:done are unchanged either way.
type ChatReplyAlertGate func(ctx context.Context, inputTaskID, content string) bool

var chatReplyAlertGate atomic.Pointer[ChatReplyAlertGate]

// SetChatReplyAlertGate installs gate for every TaskService in the process;
// nil removes it. Without a gate every reply raises the alert, as upstream.
func SetChatReplyAlertGate(gate ChatReplyAlertGate) {
	if gate == nil {
		chatReplyAlertGate.Store(nil)
		return
	}
	chatReplyAlertGate.Store(&gate)
}

// chatReplyAlertAllowed is the hook broadcastChatDone asks before it attaches
// the alert to a reply.
func chatReplyAlertAllowed(ctx context.Context, task db.AgentTaskQueue, msg *db.ChatMessage) bool {
	gate := chatReplyAlertGate.Load()
	if gate == nil || msg == nil {
		return true
	}
	return (*gate)(ctx, util.UUIDToString(chatInputOwnerID(task)), msg.Content)
}
