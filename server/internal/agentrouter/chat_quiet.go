// Added by the mouse9527/multica fork: quiet Super Agent batch replies (M-210).

package agentrouter

import (
	"context"
	"log/slog"
	"strings"
)

// A batch turn always ends with text: the agent runtime asks for a visible
// reply when a turn ends without one, and every text reply in a chat raises
// the owner's chat reply alert. Most batches need nothing from the owner, so
// with FlagChatQuiet on a batch turn can end quietly: its reply stays in the
// chat, but raises no alert. Turns the owner starts, and batch replies that
// ask the owner to decide or act, alert as before.

const (
	// FlagChatQuiet is the feature flag (FF_ROUTER_CHAT_QUIET). It only takes
	// effect with router_chat on. Like FlagInboxRules it is declared here so
	// the upstream flag file stays untouched.
	FlagChatQuiet = "router_chat_quiet"

	// QuietReplyMarker, as the first line of a batch turn's reply, says the
	// batch needs nothing from the owner.
	QuietReplyMarker = "[无需你处理]"

	// runtimeErrorPrefix opens the agent runtime's own error text ("API
	// Error: 504 ..."), which a turn can end with in place of a reply.
	runtimeErrorPrefix = "API Error:"
)

// quietReply reports whether a batch turn's reply raises no alert: it opens
// with QuietReplyMarker, or it is only the runtime's error text.
func quietReply(content string) bool {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, runtimeErrorPrefix) {
		return true
	}
	first, _, _ := strings.Cut(content, "\n")
	return strings.Trim(first, " \t\r*_`>") == QuietReplyMarker
}

// ChatReplyAlert is the service.ChatReplyAlertGate for Super Agent chats:
// only a batch turn's quiet reply loses the alert. A turn is a batch turn
// when its input is a router batch alone; any member message in the input
// makes it the owner's turn. A lookup error keeps the alert.
func (s *Store) ChatReplyAlert(ctx context.Context, inputTaskID, content string) bool {
	if inputTaskID == "" || !quietReply(content) {
		return true
	}
	var batchTurn bool
	err := s.DB.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM chat_message WHERE task_id = $1::uuid AND message_kind = 'router_batch')
		   AND NOT EXISTS (SELECT 1 FROM chat_message WHERE task_id = $1::uuid AND role = 'user' AND message_kind <> 'router_batch')`,
		inputTaskID).Scan(&batchTurn)
	if err != nil {
		slog.Warn("router: chat reply alert lookup failed; keeping the alert", "input_task_id", inputTaskID, "error", err)
		return true
	}
	return !batchTurn
}
