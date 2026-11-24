// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

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

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MountAgentMCPOAuth serves /api/agents/{id}/mcp-oauth: the agent's OAuth
// MCP servers and its owner's connection to each. 404 unless mcp_oauth_v1 is
// on. Configuring takes the agent owner or a workspace owner/admin, as for
// the agent's other MCP servers; connecting takes the agent owner, because
// phase 1 runs the agent with its owner's grant. Every write takes a signed-in
// session (requireInteractiveSession): no token of any kind may attach or
// detach an identity.
func (h *Handler) MountAgentMCPOAuth(r chi.Router) {
	r.Use(h.requireMCPOAuthV1)
	r.Get("/", h.ListAgentMCPOAuthServers)
	r.With(requireInteractiveSession).Post("/", h.CreateAgentMCPOAuthServer)
	r.With(requireInteractiveSession).Delete("/{serverId}", h.DeleteAgentMCPOAuthServer)
	r.With(requireInteractiveSession).Post("/{serverId}/connect", h.ConnectAgentMCPOAuthServer)
	r.With(requireInteractiveSession).Delete("/{serverId}/connection", h.DisconnectAgentMCPOAuthServer)
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

type mcpOAuthServerResponse struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	Scope           string    `json:"scope"`
	HasExtraHeaders bool      `json:"has_extra_headers"`
	CreatedAt       time.Time `json:"created_at"`
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

func mcpOAuthServerJSON(server mcpoauth.Server, connection *mcpoauth.ConnectionStatus) mcpOAuthServerResponse {
	out := mcpOAuthServerResponse{
		ID: server.ID, Name: server.Name, URL: server.URL, Scope: server.Scope,
		HasExtraHeaders: server.HasExtraHeaders, CreatedAt: server.CreatedAt,
	}
	if connection != nil {
		out.Connection = &mcpOAuthConnectionResponse{
			Status: connection.Status, ExpiresAt: connection.ExpiresAt, LastError: connection.LastError,
			ConnectedAt: connection.ConnectedAt, RefreshedAt: connection.RefreshedAt,
		}
	}
	return out
}

// ListAgentMCPOAuthServers returns the agent's OAuth MCP servers and the
// owner's connection to each. Extra header values and tokens are never part
// of it.
func (h *Handler) ListAgentMCPOAuthServers(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	workspaceID, agentID, ownerID := uuidToString(agent.WorkspaceID), uuidToString(agent.ID), uuidToString(agent.OwnerID)
	servers, err := store.ListServersForAgent(r.Context(), workspaceID, agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list MCP OAuth servers")
		return
	}
	statuses, err := store.ListConnectionStatusesForAgent(r.Context(), workspaceID, agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list MCP OAuth servers")
		return
	}
	owned := map[string]*mcpoauth.ConnectionStatus{}
	for i := range statuses {
		if statuses[i].UserID == ownerID {
			owned[statuses[i].ServerID] = &statuses[i]
		}
	}
	resp := mcpOAuthListResponse{Servers: make([]mcpOAuthServerResponse, 0, len(servers)), CanConnect: agent.OwnerID.Valid && ownerID == requestUserID(r)}
	for _, server := range servers {
		resp.Servers = append(resp.Servers, mcpOAuthServerJSON(server, owned[server.ID]))
	}
	writeJSON(w, http.StatusOK, resp)
}

type createMCPOAuthServerRequest struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Scope        string            `json:"scope"`
	ExtraHeaders map[string]string `json:"extra_headers"`
}

func (h *Handler) CreateAgentMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	var req createMCPOAuthServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	server, err := store.CreateServer(r.Context(), mcpoauth.ServerInput{
		WorkspaceID:  uuidToString(agent.WorkspaceID),
		AgentID:      uuidToString(agent.ID),
		CreatedBy:    requestUserID(r),
		Name:         strings.TrimSpace(req.Name),
		URL:          strings.TrimSpace(req.URL),
		Scope:        strings.TrimSpace(req.Scope),
		ExtraHeaders: req.ExtraHeaders,
	})
	switch {
	case errors.Is(err, mcpoauth.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, mcpoauth.ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to create the MCP OAuth server")
	default:
		writeJSON(w, http.StatusCreated, mcpOAuthServerJSON(server, nil))
	}
}

// agentMCPOAuthServer loads the server named in the path and checks it
// belongs to the agent in the path.
func agentMCPOAuthServer(w http.ResponseWriter, r *http.Request, store *mcpoauth.Store, agent db.Agent) (mcpoauth.Server, bool) {
	serverID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "serverId"), "server id")
	if !ok {
		return mcpoauth.Server{}, false
	}
	server, err := store.GetServer(r.Context(), uuidToString(agent.WorkspaceID), uuidToString(serverID))
	if errors.Is(err, mcpoauth.ErrNotFound) || (err == nil && server.AgentID != uuidToString(agent.ID)) {
		writeError(w, http.StatusNotFound, "MCP OAuth server not found")
		return mcpoauth.Server{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load the MCP OAuth server")
		return mcpoauth.Server{}, false
	}
	return server, true
}

// DeleteAgentMCPOAuthServer removes the server and every connection to it.
func (h *Handler) DeleteAgentMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	server, ok := agentMCPOAuthServer(w, r, store, agent)
	if !ok {
		return
	}
	if err := store.DeleteServer(r.Context(), server.WorkspaceID, server.ID); err != nil && !errors.Is(err, mcpoauth.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "failed to delete the MCP OAuth server")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type connectMCPOAuthRequest struct {
	// ReturnTo is the app path to go back to once connected.
	ReturnTo string `json:"return_to"`
}

type connectMCPOAuthResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

// ConnectAgentMCPOAuthServer starts the authorization code flow for the
// agent owner and returns the URL to open. Only the owner, in person.
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
	server, ok := agentMCPOAuthServer(w, r, store, agent)
	if !ok {
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
	authorizationURL, err := mcpoauth.Flow{Store: store}.Start(r.Context(), server, userID, redirectURI, safeReturnPath(req.ReturnTo))
	if err != nil {
		h.writeMCPOAuthFlowError(w, server.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, connectMCPOAuthResponse{AuthorizationURL: authorizationURL})
}

// safeReturnPath keeps only an app-relative path, so the page that finishes a
// connect cannot be made to send the browser to another site.
func safeReturnPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) > 2048 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.HasPrefix(path, "/\\") || strings.ContainsAny(path, "\r\n") {
		return ""
	}
	return path
}

// writeMCPOAuthFlowError answers with the flow's own message, which is safe
// for the user, and logs the cause. Causes carry hosts and HTTP statuses,
// never a token: the token endpoint's body is not read on failure.
func (h *Handler) writeMCPOAuthFlowError(w http.ResponseWriter, serverID string, err error) {
	for _, known := range []error{mcpoauth.ErrDiscovery, mcpoauth.ErrRegistration, mcpoauth.ErrNeedsPreregistered,
		mcpoauth.ErrUnsupportedClient, mcpoauth.ErrExchange} {
		if errors.Is(err, known) {
			slog.Warn("mcp oauth: connect step failed", "server_id", serverID, "error", err)
			writeErrorCode(w, http.StatusBadGateway, "mcp_oauth_upstream", known.Error())
			return
		}
	}
	slog.Error("mcp oauth: connect failed", "server_id", serverID, "error", err)
	writeError(w, http.StatusInternalServerError, "failed to connect the MCP server")
}

// DisconnectAgentMCPOAuthServer forgets the owner's grant for the server and
// cancels the owner's connects to it still in flight. The authorization
// servers seen so far advertise no revocation endpoint, so the tokens are
// dropped here and expire at the server on their own.
func (h *Handler) DisconnectAgentMCPOAuthServer(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return
	}
	store, ok := h.mcpOAuthStore(w)
	if !ok {
		return
	}
	server, ok := agentMCPOAuthServer(w, r, store, agent)
	if !ok {
		return
	}
	if !agent.OwnerID.Valid {
		// An agent without an owner has no connection to forget.
		writeError(w, http.StatusNotFound, "this agent has no owner, so it has no connection")
		return
	}
	err := store.DeleteConnection(r.Context(), server.WorkspaceID, server.ID, uuidToString(agent.OwnerID))
	if err != nil && !errors.Is(err, mcpoauth.ErrNotConnected) {
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
	AgentID     string `json:"agent_id"`
	ServerID    string `json:"server_id"`
	ReturnTo    string `json:"return_to"`
}

var errMCPOAuthNoLongerAllowed = errors.New("you can no longer connect this agent")

// CompleteMCPOAuth finishes a connect started by the same user. Before the
// code is exchanged it checks that the server still exists and that the user
// is still a member of its workspace and the owner of its agent.
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
	var agentID string
	pending, err := mcpoauth.Flow{Store: store}.Complete(r.Context(), req.State, code, userID, func(p mcpoauth.PendingAuthorization) error {
		server, err := store.GetServer(r.Context(), p.WorkspaceID, p.ServerID)
		if err != nil {
			return errMCPOAuthNoLongerAllowed
		}
		workspaceUUID, err := parseUUIDStrict(p.WorkspaceID)
		if err != nil {
			return errMCPOAuthNoLongerAllowed
		}
		agentUUID, err := parseUUIDStrict(server.AgentID)
		if err != nil {
			return errMCPOAuthNoLongerAllowed
		}
		userUUID, err := parseUUIDStrict(userID)
		if err != nil {
			return errMCPOAuthNoLongerAllowed
		}
		if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
			UserID: userUUID, WorkspaceID: workspaceUUID,
		}); err != nil {
			return errMCPOAuthNoLongerAllowed
		}
		agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: agentUUID, WorkspaceID: workspaceUUID})
		if err != nil || !agent.OwnerID.Valid || uuidToString(agent.OwnerID) != userID {
			return errMCPOAuthNoLongerAllowed
		}
		agentID = server.AgentID
		return nil
	})
	switch {
	case errors.Is(err, mcpoauth.ErrStateInvalid):
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_state_invalid", err.Error())
	case errors.Is(err, errMCPOAuthNoLongerAllowed):
		writeErrorCode(w, http.StatusForbidden, "mcp_oauth_not_allowed", err.Error())
	case errors.Is(err, mcpoauth.ErrAuthorizationDenied):
		writeErrorCode(w, http.StatusBadRequest, "mcp_oauth_denied", err.Error())
	case err != nil:
		h.writeMCPOAuthFlowError(w, pending.ServerID, err)
	default:
		writeJSON(w, http.StatusOK, completeMCPOAuthResponse{
			WorkspaceID: pending.WorkspaceID, AgentID: agentID, ServerID: pending.ServerID, ReturnTo: pending.ReturnTo,
		})
	}
}
