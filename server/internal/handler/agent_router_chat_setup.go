// Added by the mouse9527/multica fork: Router settings by instruction (M-105).
//
// An owner can set a Router up by telling the chat's agent ("make this chat
// a Router, subscribe project X"). The agent's CLI resolves "this chat" with
// GET /api/router/chats/current and then uses the ordinary subscription
// API. Binding a chat from an agent run follows the S2 delegation rule: the
// run's member originator is the authority, and it must be the chat's
// creator, so only a run the owner started in their own chat can make it a
// Router. Router batch turns have no originator and cannot.

package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/featureflags"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GetCurrentRouterChat returns the chat session of the calling agent's live
// chat run.
func (h *Handler) GetCurrentRouterChat(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if !featureflags.RouterChatEnabled(r.Context(), h.FeatureFlags) {
		writeError(w, http.StatusBadRequest, "Router chats need the router_chat feature")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	task, ok := h.taskFromRequestHeader(r)
	if actorType != "agent" || !ok || uuidToString(task.AgentID) != actorID || isTerminalTaskStatus(task.Status) {
		writeError(w, http.StatusBadRequest, "the current chat is only known inside a running chat run")
		return
	}
	if !task.ChatSessionID.Valid {
		writeError(w, http.StatusBadRequest, "this run is not a chat run, so there is no current chat")
		return
	}
	chat, err := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{
		ID: task.ChatSessionID, WorkspaceID: parseUUID(workspaceID),
	})
	if err != nil {
		if writeQueryCancellation(w, r, err) {
			return
		}
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_session_id": uuidToString(chat.ID),
		"title":           chat.Title,
		"agent_id":        uuidToString(chat.AgentID),
		"creator_id":      uuidToString(chat.CreatorID),
		"status":          chat.Status,
	})
}

// agentMayBindChat lets an agent bind chatID only on the authority of its
// run's member originator, who must have started that chat. It writes the
// error and returns false otherwise.
func (h *Handler) agentMayBindChat(w http.ResponseWriter, r *http.Request, auth routerAuthority, workspaceID, chatID string) bool {
	chat, err := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{
		ID: parseUUID(chatID), WorkspaceID: parseUUID(workspaceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "chat session "+chatID+" not found in this workspace")
		return false
	}
	if err != nil {
		if !writeQueryCancellation(w, r, err) {
			writeError(w, http.StatusInternalServerError, "router request failed")
		}
		return false
	}
	if auth.userID == "" || uuidToString(chat.CreatorID) != auth.userID {
		writeError(w, http.StatusForbidden, "an agent can bind a chat to a Router only in a run started by that chat's creator")
		return false
	}
	return true
}
