// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175).

package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/featureflags"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// withSuperAgentChatNote appends agentrouter's Super Agent note to the
// instructions of a turn in a chat that subscriptions are bound to, so a turn
// the owner starts knows the chat is a Super Agent and what its scopes are.
// Issue tasks, other chats, and router_chat off get instructions unchanged; so
// does a failed read, which must not cost the owner their turn.
func (h *Handler) withSuperAgentChatNote(ctx context.Context, task *db.AgentTaskQueue, workspaceID pgtype.UUID, instructions string) string {
	if !task.ChatSessionID.Valid || !featureflags.RouterChatEnabled(ctx, h.FeatureFlags) {
		return instructions
	}
	note, err := h.routerStore().ChatTurnNote(ctx, uuidToString(workspaceID), uuidToString(task.ChatSessionID))
	if err != nil {
		slog.Warn("router: super agent chat note failed; claiming without it",
			"task_id", uuidToString(task.ID), "chat_session_id", uuidToString(task.ChatSessionID), "error", err)
		return instructions
	}
	if note == "" {
		return instructions
	}
	if instructions == "" {
		return note
	}
	return instructions + "\n\n" + note
}
