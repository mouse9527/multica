// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// OAuth is an auth mode of an MCP entry (mcpoauth.AuthTypeOAuth): the entries
// themselves are configured through the agent's mcp_config and the workspace
// MCP library as any other. These routes only show and change connections.
// Every write takes a signed-in session (requireInteractiveSession): no token
// of any kind may attach or detach an identity. All are 404 unless
// mcp_oauth_v1 is on.

// MountAgentMCPOAuth serves /api/agents/{id}/mcp-oauth: the OAuth entries the
// agent runs with and its owner's connection to each. Listing takes the agent
// owner or a workspace owner/admin, as the agent's other MCP settings do;
// connecting takes the agent owner, because tasks run with the owner's grant.
func (h *Handler) MountAgentMCPOAuth(r chi.Router) {
	r.Use(h.requireMCPOAuthV1)
	r.Get("/", h.ListAgentMCPOAuthServers)
	r.With(requireInteractiveSession).Post("/{name}/connect", h.ConnectAgentMCPOAuthServer)
	r.With(requireInteractiveSession).Delete("/{name}/connection", h.DisconnectAgentMCPOAuthServer)
}

// MountWorkspaceMCPOAuth serves /api/workspaces/{id}/mcp-oauth: the
// workspace library's OAuth entries and the caller's own connection to each.
// Any member connects for themselves; the connection is then used by every
// agent of theirs the entry is bound to.
func (h *Handler) MountWorkspaceMCPOAuth(r chi.Router) {
	r.Use(h.requireMCPOAuthV1)
	r.Get("/", h.ListWorkspaceMCPOAuthServers)
	r.With(requireInteractiveSession).Post("/{serverId}/connect", h.ConnectWorkspaceMCPOAuthServer)
	r.With(requireInteractiveSession).Delete("/{serverId}/connection", h.DisconnectWorkspaceMCPOAuthServer)
}

// MountMCPOAuth serves /api/mcp-oauth, the user-scoped end of a connect: the
// web page at mcpoauth.CallbackPath posts the code here with the session of
// whoever it was that the authorization server sent back.
func (h *Handler) MountMCPOAuth(r chi.Router) {
	r.Use(h.requireMCPOAuthV1)
	r.With(requireInteractiveSession).Post("/complete", h.CompleteMCPOAuth)
}

// requireInteractiveSession admits only a signed-in session: the credential
// the auth middleware authenticated the request with must be a session JWT
// signed by this server whose subject is the request's user, whether it came
// in the auth cookie or in the Authorization header. It is an allowlist: a
// local PAT (mul_), a cloud PAT, a task token, or any credential added later
// is refused unless it is that.
//
// RequireHumanActor is not enough here: it refuses task tokens and cloud
// PATs, but the auth middleware marks no actor source for a local PAT, so a
// script holding one would pass as the user. And the check reads the
// credential the middleware used, not just the Authorization header: the
// middleware also accepts a PAT placed in the auth cookie, and a PAT holder
// can compute that cookie's token-bound CSRF value from the PAT itself.
// Connecting puts a person's identity behind an agent, so it takes the person.
func requireInteractiveSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMachineCredentialActor(r) || !isSessionFor(authenticatedCredential(r), requestUserID(r)) {
			writeErrorCode(w, http.StatusForbidden, "mcp_oauth_session_only", "this action needs a signed-in session, not a token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isSessionFor reports whether credential is a valid session JWT of this
// server (HS256, signed with its secret, not expired) for userID. The auth
// middleware has verified it already; this re-check is what makes the guard
// an allowlist rather than a list of token shapes to refuse.
func isSessionFor(credential, userID string) bool {
	if credential == "" || userID == "" {
		return false
	}
	token, err := jwt.Parse(credential, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return auth.JWTSecret(), nil
	})
	if err != nil || !token.Valid {
		return false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	sub, _ := claims["sub"].(string)
	return sub != "" && sub == userID
}

// authenticatedCredential is the credential the auth middleware authenticated
// the request with, chosen by the middleware's own rule (extractToken): an
// "Authorization: Bearer" header first, otherwise the auth cookie.
// TestMCPOAuthSessionGuardMatchesTheAuthMiddleware sends requests through the
// real middleware, so the two cannot drift apart unnoticed.
func authenticatedCredential(r *http.Request) string {
	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := strings.CutPrefix(header, "Bearer "); ok {
			return token
		}
	}
	if cookie, err := r.Cookie(auth.AuthCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

func (h *Handler) requireMCPOAuthV1(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !featureflags.MCPOAuthV1Enabled(r.Context(), h.FeatureFlags) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type mcpOAuthDatabase struct {
	dbExecutor
	txStarter
}

// mcpOAuthBoxCache holds the box built from the deployment key, rebuilt only
// when the key's value changes, so requests do not redo the AES key schedule.
// It reads the environment rather than being set at startup so the server
// wiring needs no new field for a feature that is off by default.
var mcpOAuthBoxCache struct {
	sync.Mutex
	raw string
	box *secretbox.Box
}

func mcpOAuthBox() (*secretbox.Box, error) {
	raw := os.Getenv(mcpoauth.SecretKeyEnv)
	mcpOAuthBoxCache.Lock()
	defer mcpOAuthBoxCache.Unlock()
	if mcpOAuthBoxCache.box != nil && mcpOAuthBoxCache.raw == raw {
		return mcpOAuthBoxCache.box, nil
	}
	key, err := secretbox.LoadKey(mcpoauth.SecretKeyEnv)
	if err != nil {
		return nil, err
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, err
	}
	mcpOAuthBoxCache.raw, mcpOAuthBoxCache.box = raw, box
	return box, nil
}

// mcpOAuthStore opens the store with the deployment key. Without the key the
// feature is unavailable rather than storing anything unsealed.
func (h *Handler) mcpOAuthStore(w http.ResponseWriter) (*mcpoauth.Store, bool) {
	box, err := mcpOAuthBox()
	if err == nil {
		store, storeErr := mcpoauth.NewStore(mcpOAuthDatabase{h.DB, h.TxStarter}, box)
		if storeErr == nil {
			return store, true
		}
	}
	writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_not_configured", "MCP OAuth is not configured on this server")
	return nil, false
}

type mcpOAuthConnectionResponse struct {
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	ConnectedAt time.Time  `json:"connected_at"`
	RefreshedAt *time.Time `json:"refreshed_at,omitempty"`
}

func mcpOAuthConnectionJSON(status *mcpoauth.ConnectionStatus, entryURL string) *mcpOAuthConnectionResponse {
	if status == nil {
		return nil
	}
	current := status.ForURL(entryURL)
	return &mcpOAuthConnectionResponse{
		Status: current.Status, ExpiresAt: current.ExpiresAt, LastError: current.LastError,
		ConnectedAt: current.ConnectedAt, RefreshedAt: current.RefreshedAt,
	}
}

// mcpOAuthProblem is why an entry cannot be connected, for the settings page.
// It is the validation message, which names fields and never values.
func mcpOAuthProblem(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type mcpOAuthServerResponse struct {
	// Name is the entry's name in the agent's MCP config, and the path key
	// of connect and disconnect.
	Name string `json:"name"`
	// Source is "agent" for an entry of the agent's own mcp_config and
	// "workspace" for a workspace library entry bound to the agent.
	Source            string `json:"source"`
	WorkspaceServerID string `json:"workspace_server_id,omitempty"`
	Scope             string `json:"scope"`
	// Shared: the connection is the owner's one connection to this
	// workspace entry, used by every agent of theirs it is bound to.
	Shared bool `json:"shared"`
	// Problem is why the entry cannot be connected; empty when it can.
	Problem string `json:"problem,omitempty"`
	// Connection is the agent owner's connection, the one tasks use. Null
	// until the owner connects.
	Connection *mcpOAuthConnectionResponse `json:"connection"`
}

type mcpOAuthListResponse struct {
	Servers []mcpOAuthServerResponse `json:"servers"`
	// CanConnect is whether the caller may press "connect": only the agent
	// owner can.
	CanConnect bool `json:"can_connect"`
}

// ListAgentMCPOAuthServers returns the OAuth entries the agent runs with —
// its own and its bound workspace entries — and the owner's connection to
// each. URLs and tokens are never part of it.
func (h *Handler) ListAgentMCPOAuthServers(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	entries, err := h.agentMCPOAuthEntries(r.Context(), agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list MCP OAuth servers")
		return
	}
	ownerID := uuidToString(agent.OwnerID)
	statuses := map[string]mcpoauth.ConnectionStatus{}
	if agent.OwnerID.Valid {
		refs := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Ref != "" {
				refs = append(refs, entry.Ref)
			}
		}
		if statuses, err = store.ListConnectionStatuses(r.Context(), uuidToString(agent.WorkspaceID), ownerID, refs); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list MCP OAuth servers")
			return
		}
	}
	resp := mcpOAuthListResponse{Servers: make([]mcpOAuthServerResponse, 0, len(entries)), CanConnect: agent.OwnerID.Valid && ownerID == requestUserID(r)}
	for _, entry := range entries {
		item := mcpOAuthServerResponse{
			Name: entry.Name, Source: entry.Source, WorkspaceServerID: entry.WorkspaceServerID,
			Scope: entry.Entry.Scope, Shared: entry.Source == mcpOAuthSourceWorkspace, Problem: mcpOAuthProblem(entry.Err),
		}
		if status, ok := statuses[entry.Ref]; ok && entry.Ref != "" {
			item.Connection = mcpOAuthConnectionJSON(&status, entry.Entry.URL)
		}
		resp.Servers = append(resp.Servers, item)
	}
	writeJSON(w, http.StatusOK, resp)
}

// agentMCPOAuthEntry finds the OAuth entry named in the path among those the
// agent runs with.
func (h *Handler) agentMCPOAuthEntry(w http.ResponseWriter, r *http.Request, agent db.Agent) (mcpOAuthEntry, bool) {
	name := chi.URLParam(r, "name")
	entries, err := h.agentMCPOAuthEntries(r.Context(), agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load the agent's MCP configuration")
		return mcpOAuthEntry{}, false
	}
	for _, entry := range entries {
		if entry.Name == name && entry.Ref != "" {
			return entry, true
		}
	}
	writeError(w, http.StatusNotFound, "MCP OAuth server not found")
	return mcpOAuthEntry{}, false
}

type connectMCPOAuthRequest struct {
	// ReturnTo is the app path to go back to once connected.
	ReturnTo string `json:"return_to"`
}

type connectMCPOAuthResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

// ConnectAgentMCPOAuthServer starts the authorization code flow for the
// agent owner and returns the URL to open. Only the owner, in person. For a
// workspace entry this is the owner's one connection to it, the same one the
// workspace MCP settings connect.
func (h *Handler) ConnectAgentMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	userID := requestUserID(r)
	if !agent.OwnerID.Valid || uuidToString(agent.OwnerID) != userID {
		writeErrorCode(w, http.StatusForbidden, "mcp_oauth_owner_only", "only the agent's owner can connect it to an MCP server")
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	entry, ok := h.agentMCPOAuthEntry(w, r, agent)
	if !ok {
		return
	}
	h.startMCPOAuthConnect(w, r, store, entry.Err, mcpoauth.Target{
		WorkspaceID: uuidToString(agent.WorkspaceID), Ref: entry.Ref, URL: entry.Entry.URL, Scope: entry.Entry.Scope,
	}, userID)
}

// startMCPOAuthConnect runs Flow.Start for userID and answers with the URL to
// open.
func (h *Handler) startMCPOAuthConnect(w http.ResponseWriter, r *http.Request, store *mcpoauth.Store, entryErr error, target mcpoauth.Target, userID string) {
	if entryErr != nil {
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_invalid_entry", entryErr.Error())
		return
	}
	redirectURI, err := mcpoauth.RedirectURI(h.cfg.AppURL)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "mcp_oauth_not_configured", err.Error())
		return
	}
	var req connectMCPOAuthRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	authorizationURL, err := mcpoauth.Flow{Store: store}.Start(r.Context(), target, userID, redirectURI, safeReturnPath(req.ReturnTo))
	if err != nil {
		h.writeMCPOAuthFlowError(w, target.Ref, err)
		return
	}
	writeJSON(w, http.StatusOK, connectMCPOAuthResponse{AuthorizationURL: authorizationURL})
}

// DisconnectAgentMCPOAuthServer forgets the owner's grant for the entry and
// cancels the owner's connects to it still in flight. For a workspace entry
// that is the owner's one connection, so every agent of theirs it is bound to
// loses it. The authorization servers seen so far advertise no revocation
// endpoint, so the tokens are dropped here and expire at the server on their
// own.
func (h *Handler) DisconnectAgentMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	entry, ok := h.agentMCPOAuthEntry(w, r, agent)
	if !ok {
		return
	}
	if !agent.OwnerID.Valid {
		// An agent without an owner has no connection to forget.
		writeError(w, http.StatusNotFound, "this agent has no owner, so it has no connection")
		return
	}
	err := store.DeleteConnection(r.Context(), uuidToString(agent.WorkspaceID), entry.Ref, uuidToString(agent.OwnerID))
	if err != nil && !errors.Is(err, mcpoauth.ErrNotConnected) {
		writeError(w, http.StatusInternalServerError, "failed to disconnect the MCP server")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type workspaceMCPOAuthServerResponse struct {
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	// Problem is why the entry cannot be connected; empty when it can.
	Problem string `json:"problem,omitempty"`
	// Connection is the caller's own connection, shared by every agent of
	// theirs the entry is bound to. Null until they connect.
	Connection *mcpOAuthConnectionResponse `json:"connection"`
}

type workspaceMCPOAuthListResponse struct {
	Servers []workspaceMCPOAuthServerResponse `json:"servers"`
}

// workspaceMCPOAuthEntry is one OAuth entry of the workspace library.
type workspaceMCPOAuthEntry struct {
	server db.WorkspaceMcpServer
	entry  mcpoauth.Entry
	err    error
}

func (e workspaceMCPOAuthEntry) target() mcpoauth.Target {
	return mcpoauth.Target{
		WorkspaceID: uuidToString(e.server.WorkspaceID), Ref: mcpoauth.WorkspaceRef(uuidToString(e.server.ID)),
		URL: e.entry.URL, Scope: e.entry.Scope,
	}
}

func parseWorkspaceMCPOAuthEntry(server db.WorkspaceMcpServer) (workspaceMCPOAuthEntry, bool) {
	if !mcpoauth.IsOAuthEntry(server.Config) {
		return workspaceMCPOAuthEntry{}, false
	}
	entry, err := mcpoauth.ParseEntry(server.Config)
	return workspaceMCPOAuthEntry{server: server, entry: entry, err: err}, true
}

// ListWorkspaceMCPOAuthServers returns the workspace library's OAuth entries
// and the caller's connection to each. Member-visible, like the library list;
// URLs and tokens are never part of it.
func (h *Handler) ListWorkspaceMCPOAuthServers(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	servers, err := h.Queries.ListWorkspaceMcpServers(r.Context(), workspaceUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list MCP OAuth servers")
		return
	}
	var entries []workspaceMCPOAuthEntry
	refs := []string{}
	for _, server := range servers {
		if entry, ok := parseWorkspaceMCPOAuthEntry(server); ok {
			entries = append(entries, entry)
			refs = append(refs, entry.target().Ref)
		}
	}
	statuses, err := store.ListConnectionStatuses(r.Context(), workspaceID, requestUserID(r), refs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list MCP OAuth servers")
		return
	}
	resp := workspaceMCPOAuthListResponse{Servers: make([]workspaceMCPOAuthServerResponse, 0, len(entries))}
	for _, entry := range entries {
		target := entry.target()
		item := workspaceMCPOAuthServerResponse{
			ServerID: uuidToString(entry.server.ID), Name: entry.server.Name, Scope: entry.entry.Scope, Problem: mcpOAuthProblem(entry.err),
		}
		if status, ok := statuses[target.Ref]; ok {
			item.Connection = mcpOAuthConnectionJSON(&status, target.URL)
		}
		resp.Servers = append(resp.Servers, item)
	}
	writeJSON(w, http.StatusOK, resp)
}

// workspaceMCPOAuthEntryFromPath loads the library entry named in the path,
// checking the caller is a member, and that the entry uses OAuth.
func (h *Handler) workspaceMCPOAuthEntryFromPath(w http.ResponseWriter, r *http.Request) (workspaceMCPOAuthEntry, bool) {
	workspaceID := workspaceIDFromURL(r, "id")
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return workspaceMCPOAuthEntry{}, false
	}
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return workspaceMCPOAuthEntry{}, false
	}
	serverUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "serverId"), "server id")
	if !ok {
		return workspaceMCPOAuthEntry{}, false
	}
	server, err := h.Queries.GetWorkspaceMcpServer(r.Context(), db.GetWorkspaceMcpServerParams{ID: serverUUID, WorkspaceID: workspaceUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return workspaceMCPOAuthEntry{}, false
	}
	entry, ok := parseWorkspaceMCPOAuthEntry(server)
	if !ok {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return workspaceMCPOAuthEntry{}, false
	}
	return entry, true
}

// ConnectWorkspaceMCPOAuthServer starts connecting the caller, in person, to
// a workspace library entry.
func (h *Handler) ConnectWorkspaceMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.workspaceMCPOAuthEntryFromPath(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	h.startMCPOAuthConnect(w, r, store, entry.err, entry.target(), requestUserID(r))
}

// DisconnectWorkspaceMCPOAuthServer forgets the caller's own grant for a
// workspace library entry, and their connects to it still in flight.
func (h *Handler) DisconnectWorkspaceMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.workspaceMCPOAuthEntryFromPath(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	target := entry.target()
	if err := store.DeleteConnection(r.Context(), target.WorkspaceID, target.Ref, requestUserID(r)); err != nil && !errors.Is(err, mcpoauth.ErrNotConnected) {
		writeError(w, http.StatusInternalServerError, "failed to disconnect the MCP server")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type completeMCPOAuthRequest struct {
	State string `json:"state"`
	Code  string `json:"code"`
	// Error is the authorization server's error parameter, when the user
	// declined. The state is still consumed.
	Error string `json:"error"`
}

type completeMCPOAuthResponse struct {
	WorkspaceID string `json:"workspace_id"`
	// AgentID is set for an agent's own entry.
	AgentID string `json:"agent_id"`
	// ServerID is set for a workspace library entry.
	ServerID  string `json:"server_id"`
	ServerRef string `json:"server_ref"`
	ReturnTo  string `json:"return_to"`
}

var errMCPOAuthNoLongerAllowed = errors.New("you can no longer connect this MCP server")

// CompleteMCPOAuth finishes a connect started by the same user. Before the
// code is exchanged it checks that the user is still a member of the
// workspace, that the entry still exists, still uses OAuth and still has the
// URL the connect started with, and, for an agent's own entry, that the user
// still owns the agent.
func (h *Handler) CompleteMCPOAuth(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	var req completeMCPOAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	code := req.Code
	if req.Error != "" {
		code = ""
	}
	var ref mcpoauth.Ref
	pending, err := mcpoauth.Flow{Store: store}.Complete(r.Context(), req.State, code, userID, func(p mcpoauth.PendingAuthorization) error {
		var err error
		ref, err = h.mcpOAuthCompletionAllowed(r, p, userID)
		return err
	})
	switch {
	case errors.Is(err, mcpoauth.ErrStateInvalid):
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_state_invalid", err.Error())
	case errors.Is(err, errMCPOAuthNoLongerAllowed):
		writeErrorCode(w, http.StatusForbidden, "mcp_oauth_not_allowed", err.Error())
	case errors.Is(err, mcpoauth.ErrAuthorizationDenied):
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_denied", err.Error())
	case err != nil:
		h.writeMCPOAuthFlowError(w, pending.ServerRef, err)
	default:
		writeJSON(w, http.StatusOK, completeMCPOAuthResponse{
			WorkspaceID: pending.WorkspaceID, AgentID: ref.AgentID, ServerID: ref.WorkspaceServerID,
			ServerRef: pending.ServerRef, ReturnTo: pending.ReturnTo,
		})
	}
}

// mcpOAuthCompletionAllowed is the check Complete runs before exchanging the
// code. Every refusal is errMCPOAuthNoLongerAllowed.
func (h *Handler) mcpOAuthCompletionAllowed(r *http.Request, p mcpoauth.PendingAuthorization, userID string) (mcpoauth.Ref, error) {
	ctx := r.Context()
	ref, ok := mcpoauth.ParseRef(p.ServerRef)
	if !ok {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	workspaceUUID, err := parseUUIDStrict(p.WorkspaceID)
	if err != nil {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	userUUID, err := parseUUIDStrict(userID)
	if err != nil {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: userUUID, WorkspaceID: workspaceUUID,
	}); err != nil {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	if ref.WorkspaceServerID != "" {
		serverUUID, err := parseUUIDStrict(ref.WorkspaceServerID)
		if err != nil {
			return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
		}
		server, err := h.Queries.GetWorkspaceMcpServer(ctx, db.GetWorkspaceMcpServerParams{ID: serverUUID, WorkspaceID: workspaceUUID})
		if err != nil {
			return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
		}
		entry, ok := parseWorkspaceMCPOAuthEntry(server)
		if !ok || entry.err != nil || entry.entry.URL != p.ServerURL {
			return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
		}
		return ref, nil
	}
	agentUUID, err := parseUUIDStrict(ref.AgentID)
	if err != nil {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentUUID, WorkspaceID: workspaceUUID})
	if err != nil || !agent.OwnerID.Valid || uuidToString(agent.OwnerID) != userID {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	entries, err := h.agentMCPOAuthEntries(ctx, agent)
	if err != nil {
		return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
	}
	for _, entry := range entries {
		if entry.Ref == p.ServerRef && entry.Err == nil && entry.Entry.URL == p.ServerURL {
			return ref, nil
		}
	}
	return mcpoauth.Ref{}, errMCPOAuthNoLongerAllowed
}

// safeReturnPath keeps only an app-relative path, so the page that finishes a
// connect cannot be made to send the browser to another site.
//
// Any control character refuses the path: browsers delete tab, CR and LF
// from a URL before parsing it, so "/\t/evil.example" would become
// "//evil.example", another site, after this check had passed.
func safeReturnPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) > 2048 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.HasPrefix(path, "/\\") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return ""
	}
	return path
}

// writeMCPOAuthFlowError answers with the flow's own message, which is safe
// for the user, and logs the cause. Causes carry hosts and HTTP statuses,
// never a token: the token endpoint's body is not read on failure.
func (h *Handler) writeMCPOAuthFlowError(w http.ResponseWriter, serverRef string, err error) {
	for _, known := range []error{mcpoauth.ErrDiscovery, mcpoauth.ErrRegistration, mcpoauth.ErrNeedsPreregistered,
		mcpoauth.ErrUnsupportedClient, mcpoauth.ErrExchange} {
		if errors.Is(err, known) {
			slog.Warn("mcp oauth: connect step failed", "server_ref", serverRef, "error", err)
			writeErrorCode(w, http.StatusBadGateway, "mcp_oauth_upstream", known.Error())
			return
		}
	}
	slog.Error("mcp oauth: connect failed", "server_ref", serverRef, "error", err)
	writeError(w, http.StatusInternalServerError, "failed to connect the MCP server")
}
