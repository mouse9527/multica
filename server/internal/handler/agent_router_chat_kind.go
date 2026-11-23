// Added by the mouse9527/multica fork: the chat message kinds the fork adds
// (M-96), kept out of upstream's switch in chat.go (M-141).

package handler

import "github.com/multica-ai/multica/server/pkg/protocol"

// forkChatMessageKind is where normalizeMessageKind sends a kind upstream
// does not know. A Router event batch keeps its kind so clients render it as
// a batch; anything else degrades to 'message' as upstream's default does.
// chat.go calls it from its default branch, one line below the cases, so a
// kind upstream adds there merges without touching the fork's line.
func forkChatMessageKind(kind string) string {
	if kind == protocol.ChatMessageKindRouterBatch {
		return protocol.ChatMessageKindRouterBatch
	}
	return protocol.ChatMessageKindMessage
}
