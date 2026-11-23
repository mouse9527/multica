// Added by the mouse9527/multica fork: the chat message kinds the fork adds
// (M-96, M-141).

package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestNormalizeMessageKindKeepsRouterBatch: the API passes a Router batch
// through with its own kind, keeps upstream's kinds, and still degrades an
// unknown or empty kind to 'message'.
func TestNormalizeMessageKindKeepsRouterBatch(t *testing.T) {
	for _, tc := range [][2]string{
		{protocol.ChatMessageKindRouterBatch, protocol.ChatMessageKindRouterBatch},
		{protocol.ChatMessageKindNoResponse, protocol.ChatMessageKindNoResponse},
		{protocol.ChatMessageKindWakeup, protocol.ChatMessageKindWakeup},
		{protocol.ChatMessageKindMessage, protocol.ChatMessageKindMessage},
		{"", protocol.ChatMessageKindMessage},
		{"some_future_kind", protocol.ChatMessageKindMessage},
	} {
		if got := normalizeMessageKind(tc[0]); got != tc[1] {
			t.Errorf("normalizeMessageKind(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}
