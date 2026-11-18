// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).
//
// HTTP surface of the Router hold (shadow mode): the recorded hold rows and
// this server process's interceptor stats. The logic is in
// internal/agentrouter/hold.go.

package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/agentrouter"
)

// ListRouterHolds lists hold rows: a member sees the items that were for
// them, an agent the rows of its own subscriptions. stats is null when the
// hold is off in the process that answered.
func (h *Handler) ListRouterHolds(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	f := agentrouter.HoldFilter{WorkspaceID: workspaceID}
	if actorType == "agent" {
		f.AgentID = actorID
	} else {
		f.RecipientUserID = actorID
	}
	if v := strings.TrimSpace(r.URL.Query().Get("subscription_id")); v != "" {
		if _, ok := parseUUIDOrBadRequest(w, v, "subscription_id"); !ok {
			return
		}
		f.SubscriptionID = v
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		f.Limit = n
	}
	out, err := h.routerStore().ListHolds(r.Context(), f)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	var stats *agentrouter.HoldStats
	if s, ok := agentrouter.CurrentHoldStats(); ok {
		stats = &s
	}
	writeJSON(w, http.StatusOK, map[string]any{"holds": out, "stats": stats})
}
