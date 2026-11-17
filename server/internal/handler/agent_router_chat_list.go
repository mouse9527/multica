// Added by the mouse9527/multica fork: Router marker in the chat list (M-108).

package handler

import (
	"net/http"

	"github.com/multica-ai/multica/server/internal/featureflags"
)

// ListRouterChats returns, in one request, a summary of every Router among
// the calling member's chats, so the chat list can mark them without asking
// per chat. Scope names in projects the member cannot see are left out.
func (h *Handler) ListRouterChats(w http.ResponseWriter, r *http.Request) {
	if !h.routerChatOn(w, r) {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	hidden, ok := h.routerHiddenProjects(w, r, workspaceID)
	if !ok {
		return
	}
	chats, err := h.routerStore().ChatSummaries(r.Context(), workspaceID, userID, hidden)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

// routerChatOn answers 404, as for an unknown route, while router_chat is off.
func (h *Handler) routerChatOn(w http.ResponseWriter, r *http.Request) bool {
	if !featureflags.RouterChatEnabled(r.Context(), h.FeatureFlags) {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}
