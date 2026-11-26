// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: grants bound to the entry's pre-registered client (M-295).

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
	// with the access token.
	Headers map[string]string `json:"headers"`
	// ExpiresAt is when the access token expires; absent when the
	// authorization server gave no lifetime.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// URL is the only URL the token may be sent to. The daemon's proxy
	// refuses a credential for any URL but its own target.
	URL string `json:"url"`
}

// ResolveMCPOAuthCredential (GET /api/daemon/tasks/{id}/mcp-oauth/{serverId}/credential[?rejected=<token hash>])
// is how a daemon's local proxy gets the current token for an OAuth MCP
// entry while the task runs, so a token that expires mid-task is replaced
// rather than failing every later call. {serverId} is the proxy handle the
// claim gave (mcpoauth.ProxyHandle: the entry's ref bound to the URL the
// proxy sends to). It serves only a task this daemon may access that is
// still dispatched or running, only an OAuth entry the task's agent runs with
// now, and only while that entry still has the URL the proxy was given: an
// entry edited during the task, and perhaps reconnected for its new URL,
// gets no token for the proxy still pointing at the old one. The grant used
// is the agent owner's, refreshed here when due.
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
	ref, proxiedURLDigest, ok := mcpoauth.ParseProxyHandle(chi.URLParam(r, "serverId"))
	if !ok {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
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
	entries, err := h.agentMCPOAuthEntries(r.Context(), agent)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_unavailable", "the agent's MCP configuration could not be read")
		return
	}
	var target *mcpoauth.Target
	for _, entry := range entries {
		if entry.Ref == ref && entry.Err == nil && !entry.Entry.Disabled {
			current := mcpoauth.NewTarget(workspaceID, ref, entry.Entry)
			target = &current
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return
	}
	if mcpoauth.URLDigest(target.URL) != proxiedURLDigest {
		writeErrorCode(w, http.StatusConflict, "mcp_oauth_entry_changed", "the MCP server's URL changed during the task; the next task uses the new one")
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
	credential, err := mcpoauth.NewTokenSource(store, mcpoauth.Refresh, slog.Default()).CredentialReplacing(r.Context(), *target, uuidToString(agent.OwnerID), rejected)
	switch {
	case errors.Is(err, mcpoauth.ErrNotConnected):
		writeErrorCode(w, http.StatusNotFound, "mcp_oauth_not_connected", "the MCP server is not connected")
		return
	case errors.Is(err, mcpoauth.ErrNeedsReconnect):
		writeErrorCode(w, http.StatusConflict, "mcp_oauth_needs_reconnect", "the MCP server needs to be reconnected")
		return
	case err != nil:
		slog.Warn("mcp oauth: credential for daemon unavailable", "task_id", uuidToString(task.ID), "server_ref", ref, "reason", mcpOAuthReason(err))
		writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_unavailable", "a current token could not be obtained")
		return
	}
	resp := mcpOAuthCredentialResponse{Headers: credential.Headers(), URL: target.URL}
	if !credential.ExpiresAt.IsZero() {
		expiresAt := credential.ExpiresAt
		resp.ExpiresAt = &expiresAt
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}
