// Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/mcpoauth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// An OAuth MCP entry may name a client registered in advance with its
// authorization server ("auth": {"client_id", "issuer",
// "token_endpoint_auth_method"}). The entry is readable configuration, so the
// client's secret is not part of it: these routes write it, sealed, into
// mcp_oauth_client, and nothing ever reads it back. A list only says whether
// one is set.
//
//	PUT|DELETE /api/agents/{id}/mcp-oauth/{name}/client-secret
//	PUT|DELETE /api/workspaces/{id}/mcp-oauth/{serverId}/client-secret
//
// Writing takes whoever may edit the entry itself, in a signed-in session.
// Every connection to the entry was made with the old client, so each write
// marks them as needing a reconnect.

// mcpOAuthClientResponse is what a list says about an entry's client. Never
// the secret, and not the client id: the lists carry no entry configuration.
type mcpOAuthClientResponse struct {
	// Preregistered: the entry names a client registered in advance rather
	// than registering one when connecting.
	Preregistered bool `json:"preregistered"`
	// ClientSecretSet: a client secret is stored for the entry.
	ClientSecretSet bool `json:"client_secret_set"`
}

func mcpOAuthClientJSON(entry mcpoauth.Entry, secretSet bool) mcpOAuthClientResponse {
	return mcpOAuthClientResponse{Preregistered: entry.Client.Preregistered(), ClientSecretSet: secretSet}
}

// mcpOAuthClientSecretsSet reports which of the entries have a client secret
// set for the client they name now.
func (h *Handler) mcpOAuthClientSecretsSet(ctx context.Context, workspaceID string, entries []mcpOAuthEntry) (map[string]bool, error) {
	refs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Ref != "" {
			refs = append(refs, entry.Ref)
		}
	}
	bindings, err := mcpoauth.ClientSecretBindings(ctx, h.DB, workspaceID, refs)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, entry := range entries {
		binding, found := bindings[entry.Ref]
		set[entry.Ref] = entry.Ref != "" && mcpoauth.SecretSetFor(binding, found, entry.Entry)
	}
	return set, nil
}

// writeMCPOAuthClientError answers the connect errors that come from the
// entry's pre-registered client settings, which only its editor can fix. It
// reports whether err was one of them.
func writeMCPOAuthClientError(w http.ResponseWriter, serverRef string, err error) bool {
	switch {
	case errors.Is(err, mcpoauth.ErrClientSecretMissing):
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_client_secret_missing", mcpoauth.ErrClientSecretMissing.Error())
	case errors.Is(err, mcpoauth.ErrClientSecretStale):
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_client_secret_stale", mcpoauth.ErrClientSecretStale.Error())
	case errors.Is(err, mcpoauth.ErrIssuerMismatch):
		slog.Warn("mcp oauth: connect step failed", "server_ref", serverRef, "error", err)
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_issuer_mismatch", mcpoauth.ErrIssuerMismatch.Error())
	default:
		return false
	}
	return true
}

type setMCPOAuthClientSecretRequest struct {
	ClientSecret string `json:"client_secret"`
}

const maxMCPOAuthClientSecretBody = 16 << 10

// SetAgentMCPOAuthClientSecret stores the client secret of one of the agent's
// own entries. A workspace entry bound to the agent is the workspace's: its
// secret is set from the workspace MCP settings.
func (h *Handler) SetAgentMCPOAuthClientSecret(w http.ResponseWriter, r *http.Request) {
	agent, entry, store, ok := h.agentMCPOAuthClientEntry(w, r)
	if !ok {
		return
	}
	h.setMCPOAuthClientSecret(w, r, store, entry.Err, entry.Entry, uuidToString(agent.WorkspaceID), entry.Ref)
}

// DeleteAgentMCPOAuthClientSecret removes it again.
func (h *Handler) DeleteAgentMCPOAuthClientSecret(w http.ResponseWriter, r *http.Request) {
	agent, entry, store, ok := h.agentMCPOAuthClientEntry(w, r)
	if !ok {
		return
	}
	h.deleteMCPOAuthClientSecret(w, r, store, uuidToString(agent.WorkspaceID), entry.Ref)
}

func (h *Handler) agentMCPOAuthClientEntry(w http.ResponseWriter, r *http.Request) (db.Agent, mcpOAuthEntry, *mcpoauth.Store, bool) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return db.Agent{}, mcpOAuthEntry{}, nil, false
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return db.Agent{}, mcpOAuthEntry{}, nil, false
	}
	entry, ok := h.agentMCPOAuthEntry(w, r, agent)
	if !ok {
		return db.Agent{}, mcpOAuthEntry{}, nil, false
	}
	if entry.Source != mcpOAuthSourceAgent {
		writeErrorCode(w, http.StatusConflict, "mcp_oauth_workspace_entry", "this is a workspace MCP entry: set its client secret in the workspace MCP settings")
		return db.Agent{}, mcpOAuthEntry{}, nil, false
	}
	return agent, entry, store, true
}

// SetWorkspaceMCPOAuthClientSecret stores the client secret of a workspace
// library entry. Only those who may edit the library may.
func (h *Handler) SetWorkspaceMCPOAuthClientSecret(w http.ResponseWriter, r *http.Request) {
	entry, store, ok := h.workspaceMCPOAuthClientEntry(w, r)
	if !ok {
		return
	}
	target := entry.target()
	h.setMCPOAuthClientSecret(w, r, store, entry.err, entry.entry, target.WorkspaceID, target.Ref)
}

// DeleteWorkspaceMCPOAuthClientSecret removes it again.
func (h *Handler) DeleteWorkspaceMCPOAuthClientSecret(w http.ResponseWriter, r *http.Request) {
	entry, store, ok := h.workspaceMCPOAuthClientEntry(w, r)
	if !ok {
		return
	}
	target := entry.target()
	h.deleteMCPOAuthClientSecret(w, r, store, target.WorkspaceID, target.Ref)
}

func (h *Handler) workspaceMCPOAuthClientEntry(w http.ResponseWriter, r *http.Request) (workspaceMCPOAuthEntry, *mcpoauth.Store, bool) {
	if !h.requireWorkspaceMcpWriter(w, r, workspaceIDFromURL(r, "id")) {
		return workspaceMCPOAuthEntry{}, nil, false
	}
	entry, ok := h.workspaceMCPOAuthEntryFromPath(w, r)
	if !ok {
		return workspaceMCPOAuthEntry{}, nil, false
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return workspaceMCPOAuthEntry{}, nil, false
	}
	return entry, store, true
}

func (h *Handler) setMCPOAuthClientSecret(w http.ResponseWriter, r *http.Request, store *mcpoauth.Store, entryErr error, entry mcpoauth.Entry, workspaceID, ref string) {
	if entryErr != nil {
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_invalid_entry", entryErr.Error())
		return
	}
	if !entry.Client.Preregistered() {
		writeErrorCode(w, http.StatusConflict, "mcp_oauth_not_preregistered", mcpoauth.ErrNotPreregistered.Error())
		return
	}
	var req setMCPOAuthClientSecretRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMCPOAuthClientSecretBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := mcpoauth.ValidateClientSecret(req.ClientSecret); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := store.SetClientSecret(r.Context(), workspaceID, ref, requestUserID(r), entry.Client, mcpoauth.NewSecret(req.ClientSecret)); err != nil {
		slog.Error("mcp oauth: set client secret failed", "server_ref", ref, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save the client secret")
		return
	}
	slog.Info("mcp oauth: client secret set", "server_ref", ref, "user_id", requestUserID(r))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteMCPOAuthClientSecret(w http.ResponseWriter, r *http.Request, store *mcpoauth.Store, workspaceID, ref string) {
	removed, err := store.DeleteClientSecret(r.Context(), workspaceID, ref)
	if err != nil {
		slog.Error("mcp oauth: delete client secret failed", "server_ref", ref, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete the client secret")
		return
	}
	if removed {
		slog.Info("mcp oauth: client secret deleted", "server_ref", ref, "user_id", requestUserID(r))
	}
	w.WriteHeader(http.StatusNoContent)
}

// settleChangedMCPOAuthClient runs after an OAuth entry was saved over its
// previous config. Connections made with another client than the entry names
// now need a reconnect. The client secret belongs to the client it was set
// for: an entry that names another client id or issuer, no longer names a
// pre-registered client, or no longer uses OAuth keeps none. It never fails
// the write; a failure is logged.
func (h *Handler) settleChangedMCPOAuthClient(ctx context.Context, workspaceID, ref string, before, after json.RawMessage) {
	if mcpoauth.ClientChanged(before, after) {
		if err := mcpoauth.MarkEntryNeedsReconnect(ctx, h.DB, workspaceID, ref, mcpoauth.ReasonClientChanged, time.Now()); err != nil {
			slog.Warn("mcp oauth: mark changed client failed", "server_ref", ref, "error", err)
		}
	}
	forget := !mcpoauth.IsOAuthEntry(after) || mcpoauth.ClientIdentityChanged(before, after)
	if !forget {
		entry, err := mcpoauth.ParseEntry(after)
		forget = err == nil && !entry.Client.Preregistered()
	}
	if forget {
		if err := mcpoauth.ForgetClientSecret(ctx, h.DB, workspaceID, ref); err != nil {
			slog.Warn("mcp oauth: forget client secret failed", "server_ref", ref, "error", err)
		}
	}
}

// settleChangedAgentMCPOAuthClients is settleChangedMCPOAuthClient for every
// OAuth entry of an agent's own mcp_config that the update kept.
func (h *Handler) settleChangedAgentMCPOAuthClients(ctx context.Context, before, after db.Agent) {
	_, previous, _ := mcpServersOf(before.McpConfig)
	_, current, ok := mcpServersOf(after.McpConfig)
	if !ok {
		return
	}
	workspaceID, agentID := uuidToString(after.WorkspaceID), uuidToString(after.ID)
	for name, raw := range current {
		if !mcpoauth.IsOAuthEntry(raw) || !mcpoauth.ValidEntryName(name) {
			continue
		}
		h.settleChangedMCPOAuthClient(ctx, workspaceID, mcpoauth.AgentRef(agentID, name), previous[name], raw)
	}
}

// workspaceMCPOAuthConfigBefore is the library entry's config before an
// update replaces it; nil when the update keeps the config or it cannot be
// read.
func (h *Handler) workspaceMCPOAuthConfigBefore(ctx context.Context, params db.UpdateWorkspaceMcpServerParams) json.RawMessage {
	if len(params.Config) == 0 {
		return nil
	}
	server, err := h.Queries.GetWorkspaceMcpServer(ctx, db.GetWorkspaceMcpServerParams{ID: params.ID, WorkspaceID: params.WorkspaceID})
	if err != nil {
		return nil
	}
	return json.RawMessage(server.Config)
}

// settleChangedWorkspaceMCPOAuthClient runs after a library entry's config
// was replaced; see settleChangedMCPOAuthClient.
func (h *Handler) settleChangedWorkspaceMCPOAuthClient(ctx context.Context, before json.RawMessage, server db.WorkspaceMcpServer) {
	if before == nil {
		return
	}
	h.settleChangedMCPOAuthClient(ctx, uuidToString(server.WorkspaceID), mcpoauth.WorkspaceRef(uuidToString(server.ID)), before, json.RawMessage(server.Config))
}
