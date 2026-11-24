// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175), and the chat layer that owns all Super Agent prompt content (M-250).

package handler

import (
	"context"
	"log/slog"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// withSuperAgentChatLayer adds the chat layer to a claimed task's instructions.
// Everything a prompt says about Super Agents comes from here, never from the
// agent's own prompt (M-250), and only chat turns get it:
//
//   - A turn in a chat subscriptions are bound to (router_chat on) gets the
//     Super Agent note, so a turn the owner starts knows the chat is a Super
//     Agent and what its scopes are. A system agent whose built-in task rules
//     conflict with that role runs on its identity and notes only
//     (service.SuperAgentTurnInstructions).
//   - A turn in any other chat gets the setup pointer, for the agents that
//     have one (Mika, router_v1 on).
//
// Issue tasks get instructions unchanged; so does a failed note read, which
// must not cost the owner their turn.
func (h *Handler) withSuperAgentChatLayer(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, instructions string) string {
	if !task.ChatSessionID.Valid {
		return instructions
	}
	if note := h.superAgentChatNote(ctx, task, agent); note != "" {
		if own, ok := service.SuperAgentTurnInstructions(agent.SystemKey.String, agent.Name, agent.Instructions); ok {
			instructions = own
		}
		return joinPromptLayers(instructions, note)
	}
	return joinPromptLayers(instructions, h.TaskService.SuperAgentSetupPointer(ctx, agent.SystemKey.String))
}

// superAgentChatNote is agentrouter's Super Agent note for the task's chat,
// or "" when router_chat is off, no subscription is bound to the chat, or the
// read fails.
func (h *Handler) superAgentChatNote(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent) string {
	if !featureflags.RouterChatEnabled(ctx, h.FeatureFlags) {
		return ""
	}
	note, err := h.routerStore().ChatTurnNote(ctx, uuidToString(agent.WorkspaceID), uuidToString(task.ChatSessionID))
	if err != nil {
		slog.Warn("router: super agent chat note failed; claiming without it",
			"task_id", uuidToString(task.ID), "chat_session_id", uuidToString(task.ChatSessionID), "error", err)
		return ""
	}
	return note
}

func joinPromptLayers(instructions, layer string) string {
	switch {
	case layer == "":
		return instructions
	case instructions == "":
		return layer
	}
	return instructions + "\n\n" + layer
}
