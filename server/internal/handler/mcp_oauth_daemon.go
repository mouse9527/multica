// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
)

var tokenHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type mcpOAuthCredentialResponse struct {
	// Headers are the request headers for the MCP server: Authorization
	// with the access token, and the server's extra headers.
	Headers map[string]string `json:"headers"`
	// ExpiresAt is when the access token expires; absent when the
	// authorization server gave no lifetime.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ResolveMCPOAuthCredential (GET /api/daemon/tasks/{id}/mcp-oauth/{serverId}/credential[?rejected=<token hash>])
// is how a daemon's local proxy gets the current token for an OAuth MCP
// server while the task runs, so a token that expires mid-task is replaced
// rather than failing every later call. It serves only a task this daemon may
// access that is still dispatched or running, and only a server of that
// task's agent, with the agent owner's grant (refreshed here when due).
func (h *Handler) ResolveMCPOAuthCredential(w http.ResponseWriter, r *http.Request) {
	if !featureflags.MCPOAuthV1Enabled(r.Context(), h.FeatureFlags) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	task, workspaceID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if task.Status != "dispatched" && task.Status != "running" {
		writeErrorCode(w, http.StatusConflict, "task_not_active", "the task is no longer running")
		return
	}
	serverUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "serverId"), "server id")
	if !ok {
		return
	}
	agent, err := h.Queries.GetAgent(r.Context(), task.AgentID)
	if err != nil {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return
	}
	if uuidToString(agent.WorkspaceID) != workspaceID || !agent.OwnerID.Valid {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return
	}
	box, err := mcpOAuthBox()
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_not_configured", "MCP OAuth is not configured on this server")
		return
	}
	store, err := mcpoauth.NewStore(mcpOAuthDatabase{h.DB, h.TxStarter}, box)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_not_configured", "MCP OAuth is not configured on this server")
		return
	}
	server, err := store.GetServer(r.Context(), workspaceID, uuidToString(serverUUID))
	if err != nil || server.AgentID != uuidToString(agent.ID) {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return
	}

	// rejected is mcpoauth.TokenHash of the token the MCP server just
	// answered 401 to: while it is still the stored token it is refreshed
	// rather than served again.
	rejected := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("rejected")))
	if rejected != "" && !tokenHashPattern.MatchString(rejected) {
		writeError(w, http.StatusBadRequest, "rejected must be a hex SHA-256")
		return
	}
	credential, err := mcpoauth.NewTokenSource(store, mcpoauth.Refresh, slog.Default()).CredentialReplacing(r.Context(), server.ID, uuidToString(agent.OwnerID), rejected)
	switch {
	case errors.Is(err, mcpoauth.ErrNotConnected):
		writeErrorCode(w, http.StatusNotFound, "mcp_oauth_not_connected", "the MCP server is not connected")
		return
	case errors.Is(err, mcpoauth.ErrNeedsReconnect):
		writeErrorCode(w, http.StatusConflict, "mcp_oauth_needs_reconnect", "the MCP server needs to be reconnected")
		return
	case err != nil:
		slog.Warn("mcp oauth: credential for daemon unavailable", "task_id", uuidToString(task.ID), "server_id", server.ID, "reason", mcpOAuthReason(err))
		writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_unavailable", "a current token could not be obtained")
		return
	}
	resp := mcpOAuthCredentialResponse{Headers: credential.Headers()}
	if !credential.ExpiresAt.IsZero() {
		expiresAt := credential.ExpiresAt
		resp.ExpiresAt = &expiresAt
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}
