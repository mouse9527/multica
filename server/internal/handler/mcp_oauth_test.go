// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: cleanup covers pre-registered client secrets (M-295).

package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// withMCPOAuth switches mcp_oauth_v1, sets a deployment key and an app URL,
// and returns a store over the same key for arranging rows directly.
func withMCPOAuth(t *testing.T, on bool) *mcpoauth.Store {
	t.Helper()
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.MCPOAuthV1, featureflag.Rule{Default: on})
	origFlags, origAppURL := testHandler.FeatureFlags, testHandler.cfg.AppURL
	testHandler.FeatureFlags = featureflag.NewService(provider)
	testHandler.cfg.AppURL = "https://app.example.test"
	t.Cleanup(func() {
		testHandler.FeatureFlags = origFlags
		testHandler.cfg.AppURL = origAppURL
	})
	key := make([]byte, secretbox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv(mcpoauth.SecretKeyEnv, base64.StdEncoding.EncodeToString(key))
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mcpoauth.NewStore(mcpOAuthDatabase{testHandler.DB, testHandler.TxStarter}, box)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// cleanupMCPOAuth removes the connections to the agent's own entries after
// the test.
func cleanupMCPOAuth(t *testing.T, agentID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		prefix := mcpoauth.AgentRefPrefix(agentID)
		testPool.Exec(ctx, `DELETE FROM mcp_oauth_state WHERE left(server_ref, length($1::text)) = $1::text`, prefix)
		testPool.Exec(ctx, `DELETE FROM mcp_oauth_connection WHERE left(server_ref, length($1::text)) = $1::text`, prefix)
		testPool.Exec(ctx, `DELETE FROM mcp_oauth_client WHERE left(server_ref, length($1::text)) = $1::text`, prefix)
	})
}

func setAgentMcpConfig(t *testing.T, agentID, config string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET mcp_config = $1 WHERE id = $2`, []byte(config), agentID); err != nil {
		t.Fatal(err)
	}
}

func mcpOAuthRouter() http.HandlerFunc {
	r := chi.NewRouter()
	r.Route("/api/agents/{id}/mcp-oauth", testHandler.MountAgentMCPOAuth)
	r.Route("/api/workspaces/{id}/mcp-oauth", testHandler.MountWorkspaceMCPOAuth)
	r.Route("/api/mcp-oauth", testHandler.MountMCPOAuth)
	return r.ServeHTTP
}

// mcpOAuthCall calls as userID signed in with a session JWT, the way the
// browser and the desktop app reach these routes.
func mcpOAuthCall(t *testing.T, userID, method, path string, body any) *testutil.Response {
	t.Helper()
	req := newRequestAs(userID, method, path, body)
	req.Header.Set("Authorization", "Bearer "+mcpOAuthSession(t, userID, time.Hour, auth.JWTSecret()))
	return testutil.Call(t, mcpOAuthRouter(), req)
}

// mcpOAuthSession is a session JWT for userID expiring in ttl, signed with key.
func mcpOAuthSession(t *testing.T, userID string, ttl time.Duration, key []byte) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID, "exp": time.Now().Add(ttl).Unix(),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func asPAT(req *http.Request) *http.Request {
	req.Header.Set("X-Actor-Source", "cloud_pat")
	return req
}

// The forms a request reaches the handler in after the auth middleware, for
// each credential. A local PAT gets no X-Actor-Source; only the
// Authorization header shows it.
var mcpOAuthCredentials = map[string]func(*http.Request){
	"local PAT": func(r *http.Request) { r.Header.Set("Authorization", "Bearer mul_0123456789abcdef") },
	"local PAT in the auth cookie": func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: "mul_0123456789abcdef"})
	},
	"cloud PAT": func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer mcn_0123456789abcdef")
		r.Header.Set("X-Actor-Source", "cloud_pat")
	},
	"task token": func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer mat_0123456789abcdef")
		r.Header.Set("X-Actor-Source", "task_token")
	},
}

const mcpOAuthTestURL = "https://mcp.example.com/mcp"

// mcpOAuthAgentFixture is a private agent (owner, plain member) whose own
// mcp_config has the OAuth entry "gateway" and a plain entry, plus the
// workspace OAuth entry "shared-gw" bound to it.
func mcpOAuthAgentFixture(t *testing.T) (agentID, ownerID, memberID, sharedID string) {
	t.Helper()
	agentID, ownerID, memberID = privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"plain":{"command":"npx"},"gateway":`+oauthEntry(mcpOAuthTestURL)+`}}`)
	sharedID, _ = workspaceOAuthEntryForTest(t, "shared-gw", "https://shared.example.com/mcp")
	bindForTest(t, agentID, sharedID)
	return agentID, ownerID, memberID, sharedID
}

// Every write takes a signed-in session; no token of any kind, the local
// mul_ PAT included, may connect, disconnect or complete.
func TestMCPOAuthWritesRefuseEveryToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, _, sharedID := mcpOAuthAgentFixture(t)
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), mcpOAuthTestURL, ownerID, "token-that-stays")
	base := "/api/agents/" + agentID + "/mcp-oauth"
	wsBase := "/api/workspaces/" + testWorkspaceID + "/mcp-oauth"

	writes := []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/gateway/connect", nil},
		{"DELETE", base + "/gateway/connection", nil},
		{"POST", wsBase + "/" + sharedID + "/connect", nil},
		{"DELETE", wsBase + "/" + sharedID + "/connection", nil},
		{"POST", "/api/mcp-oauth/complete", map[string]string{"state": "s", "code": "c"}},
	}
	for name, credential := range mcpOAuthCredentials {
		for _, write := range writes {
			req := newRequestAs(ownerID, write.method, write.path, write.body)
			credential(req)
			resp := testutil.Call(t, mcpOAuthRouter(), req).Want(http.StatusForbidden)
			if !strings.Contains(resp.Body.String(), "mcp_oauth_session_only") {
				t.Errorf("%s %s with a %s: %s", write.method, write.path, name, resp.Body.String())
			}
		}
	}
	// The connection is still there: nothing above got through.
	if _, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, mcpoauth.AgentRef(agentID, "gateway"), ownerID); err != nil {
		t.Fatalf("the connection is gone: %v", err)
	}

	// The guard is an allowlist: a JWT that is not this user's valid
	// session is refused like any token.
	for name, session := range map[string]string{
		"another user's session":     mcpOAuthSession(t, testUserID, time.Hour, auth.JWTSecret()),
		"an expired session":         mcpOAuthSession(t, ownerID, -time.Minute, auth.JWTSecret()),
		"a session signed elsewhere": mcpOAuthSession(t, ownerID, time.Hour, []byte("not-this-server's-secret-0123456789")),
		"no credential at all":       "",
	} {
		req := newRequestAs(ownerID, "DELETE", base+"/gateway/connection", nil)
		if session != "" {
			req.Header.Set("Authorization", "Bearer "+session)
		}
		if resp := testutil.Call(t, mcpOAuthRouter(), req); resp.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403: %s", name, resp.Code, resp.Body.String())
		}
	}

	// The user's own session JWT in the Authorization header (how the
	// desktop app signs in) is a session, not a token.
	mcpOAuthCall(t, ownerID, "DELETE", base+"/gateway/connection", nil).Want(http.StatusNoContent)
}

func TestMCPOAuthRoutesAreOffWithoutTheFlag(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, false)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	mcpOAuthCall(t, ownerID, "GET", "/api/agents/"+agentID+"/mcp-oauth", nil).Want(http.StatusNotFound)
	mcpOAuthCall(t, ownerID, "GET", "/api/workspaces/"+testWorkspaceID+"/mcp-oauth", nil).Want(http.StatusNotFound)
	mcpOAuthCall(t, ownerID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": "x", "code": "y"}).Want(http.StatusNotFound)
}

// The agent's list shows its own and its bound workspace OAuth entries, with
// the owner's connection to each, never a URL or a token; a connection made
// for another URL reads as needing a reconnect.
func TestMCPOAuthAgentList(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, memberID, sharedID := mcpOAuthAgentFixture(t)
	base := "/api/agents/" + agentID + "/mcp-oauth"

	mcpOAuthCall(t, memberID, "GET", base, nil).Want(http.StatusForbidden)

	var listed mcpOAuthListResponse
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if !listed.CanConnect || len(listed.Servers) != 2 {
		t.Fatalf("owner's list = %+v", listed)
	}
	gateway, shared := listed.Servers[0], listed.Servers[1]
	if gateway.Name != "gateway" || gateway.Source != "agent" || gateway.Shared || gateway.Connection != nil {
		t.Fatalf("gateway = %+v", gateway)
	}
	if shared.Name != "shared-gw" || shared.Source != "workspace" || !shared.Shared || shared.WorkspaceServerID != sharedID {
		t.Fatalf("shared = %+v", shared)
	}
	// A workspace owner may look, but connecting is the agent owner's.
	mcpOAuthCall(t, testUserID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if listed.CanConnect {
		t.Fatal("a workspace owner who is not the agent owner may connect")
	}

	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), mcpOAuthTestURL, ownerID, "access-token-never-returned")
	connectForTest(t, store, mcpoauth.WorkspaceRef(sharedID), "https://shared.example.com/mcp", ownerID, "shared-token-never-returned")
	resp := mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK)
	for _, secret := range []string{"never-returned", "mcp.example.com", "shared.example.com"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("the list carries %q: %s", secret, resp.Body.String())
		}
	}
	resp.JSON(&listed)
	for _, server := range listed.Servers {
		if server.Connection == nil || server.Connection.Status != mcpoauth.StatusConnected {
			t.Fatalf("%s: connection = %+v", server.Name, server.Connection)
		}
	}

	// The entry now points elsewhere: its connection is no longer usable.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+oauthEntry("https://moved.example.com/mcp")+`}}`)
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if c := listed.Servers[0].Connection; listed.Servers[0].Name != "gateway" || c == nil ||
		c.Status != mcpoauth.StatusNeedsReconnect || c.LastError != mcpoauth.ReasonURLChanged {
		t.Fatalf("moved gateway = %+v", listed.Servers[0])
	}
}

// An entry that is not a valid OAuth entry shows its problem and no
// connection state: there is no URL to judge a saved connection against, and
// "needs reconnecting" would send the owner to a connect that cannot work.
func TestMCPOAuthAgentListInvalidEntryHasNoConnection(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), mcpOAuthTestURL, ownerID, "token-of-a-broken-entry")
	// Saved directly, as an entry stored while the flag was off would be.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":{"url":"https://mcp.example.com/mcp","headers":{"x-api-key":"k"},"auth":{"type":"oauth"}}}}`)

	var listed mcpOAuthListResponse
	mcpOAuthCall(t, ownerID, "GET", "/api/agents/"+agentID+"/mcp-oauth", nil).Want(http.StatusOK).JSON(&listed)
	if len(listed.Servers) != 1 || listed.Servers[0].Problem == "" || listed.Servers[0].Connection != nil {
		t.Fatalf("list = %+v", listed.Servers)
	}
}

// Connecting an agent entry is the agent owner's alone, in person, and the
// SSRF boundary holds: an entry on a non-public address fails discovery.
func TestMCPOAuthConnectPermissions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	// Saved directly: the save-time check would refuse this address.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"local":`+oauthEntry("https://127.0.0.1:1/mcp")+`,`+
		`"broken":{"url":"http://mcp.example.com/mcp","auth":{"type":"oauth"}},"plain":{"command":"npx"}}}`)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	connect := base + "/local/connect"

	mcpOAuthCall(t, testUserID, "POST", connect, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, memberID, "POST", connect, nil).Want(http.StatusForbidden)
	testutil.Call(t, mcpOAuthRouter(), asPAT(newRequestAs(ownerID, "POST", connect, nil))).Want(http.StatusForbidden)
	mcpOAuthCall(t, ownerID, "POST", base+"/plain/connect", nil).Want(http.StatusNotFound)
	mcpOAuthCall(t, ownerID, "POST", base+"/missing/connect", nil).Want(http.StatusNotFound)
	if resp := mcpOAuthCall(t, ownerID, "POST", base+"/broken/connect", nil).Want(http.StatusBadRequest); !strings.Contains(resp.Body.String(), "mcp_oauth_invalid_entry") {
		t.Fatalf("connect to an invalid entry: %s", resp.Body.String())
	}

	resp := mcpOAuthCall(t, ownerID, "POST", connect, map[string]string{"return_to": "/ws/agents"}).Want(http.StatusBadGateway)
	if !strings.Contains(resp.Body.String(), "mcp_oauth_upstream") {
		t.Fatalf("connect to a non-public server: %s", resp.Body.String())
	}

	// Without an app URL there is no redirect URI to register.
	testHandler.cfg.AppURL = ""
	mcpOAuthCall(t, ownerID, "POST", connect, nil).Want(http.StatusServiceUnavailable)
}

// Any member connects a workspace entry for themselves; an entry without
// OAuth, or of another workspace, is not there to connect.
func TestMCPOAuthWorkspaceConnect(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	_, _, memberID := privateAgentTestFixture(t)
	localID, _ := workspaceOAuthEntryForTest(t, "local-gw", "https://127.0.0.1:1/mcp")
	plainID := createWorkspaceMcpServerForTest(t, "plain-gw", `{"command":"npx"}`)
	wsBase := "/api/workspaces/" + testWorkspaceID + "/mcp-oauth"

	var listed workspaceMCPOAuthListResponse
	resp := mcpOAuthCall(t, memberID, "GET", wsBase, nil).Want(http.StatusOK)
	if strings.Contains(resp.Body.String(), "127.0.0.1") {
		t.Fatalf("the list carries a URL: %s", resp.Body.String())
	}
	resp.JSON(&listed)
	found := false
	for _, server := range listed.Servers {
		if server.ServerID == plainID {
			t.Fatal("an entry without OAuth is listed")
		}
		found = found || (server.ServerID == localID && server.Name == "local-gw" && server.Connection == nil)
	}
	if !found {
		t.Fatalf("list = %+v", listed)
	}

	// The member reached the connect flow, which refuses the address.
	if resp := mcpOAuthCall(t, memberID, "POST", wsBase+"/"+localID+"/connect", nil).Want(http.StatusBadGateway); !strings.Contains(resp.Body.String(), "mcp_oauth_upstream") {
		t.Fatalf("member connect: %s", resp.Body.String())
	}
	mcpOAuthCall(t, memberID, "POST", wsBase+"/"+plainID+"/connect", nil).Want(http.StatusNotFound)
	mcpOAuthCall(t, memberID, "POST", "/api/workspaces/"+testWorkspaceID+"/mcp-oauth/00000000-0000-0000-0000-000000000001/connect", nil).Want(http.StatusNotFound)
}

// Completing checks the user against the state, the agent's current owner
// and the entry's current URL before anything is exchanged.
func TestMCPOAuthCompleteChecksWhoFinishes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, memberID, sharedID := mcpOAuthAgentFixture(t)
	verifier, err := mcpoauth.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	start := func(ref, url string) string {
		t.Helper()
		state, err := store.CreatePendingAuthorization(context.Background(), mcpoauth.PendingAuthorization{
			WorkspaceID: testWorkspaceID, ServerRef: ref, ServerURL: url, UserID: ownerID,
			Resource: url, TokenEndpoint: "https://mcp.example.com/token",
			Client: mcpoauth.Client{ID: "client"}, RedirectURI: "https://app.example.test/oauth/mcp", Verifier: verifier,
		})
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	agentRef := mcpoauth.AgentRef(agentID, "gateway")
	complete := func(userID, state, code string) *testutil.Response {
		t.Helper()
		return mcpOAuthCall(t, userID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": state, "code": code})
	}

	// Someone else's session cannot finish the owner's connect.
	complete(memberID, start(agentRef, mcpOAuthTestURL), "code").Want(http.StatusBadRequest)
	complete(ownerID, "not-a-state", "code").Want(http.StatusBadRequest)
	testutil.Call(t, mcpOAuthRouter(), asPAT(newRequestAs(ownerID, "POST", "/api/mcp-oauth/complete",
		map[string]string{"state": start(agentRef, mcpOAuthTestURL), "code": "code"}))).Want(http.StatusForbidden)

	// Declined at the authorization server: the state is used up.
	declined := start(agentRef, mcpOAuthTestURL)
	mcpOAuthCall(t, ownerID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": declined, "error": "access_denied"}).Want(http.StatusBadRequest)
	complete(ownerID, declined, "code").Want(http.StatusBadRequest)

	// The entry's URL changed between start and finish: refused before the
	// code is exchanged (the token endpoint here does not exist, so an
	// exchange would have been a 502). Same for a workspace entry.
	complete(ownerID, start(agentRef, "https://other.example.com/mcp"), "code").Want(http.StatusForbidden)
	complete(ownerID, start(mcpoauth.WorkspaceRef(sharedID), "https://other.example.com/mcp"), "code").Want(http.StatusForbidden)

	// The agent changed owner between start and finish.
	state := start(agentRef, mcpOAuthTestURL)
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET owner_id = $1 WHERE id = $2`, memberID, agentID); err != nil {
		t.Fatal(err)
	}
	complete(ownerID, state, "code").Want(http.StatusForbidden)

	if _, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, agentRef, ownerID); err == nil {
		t.Fatal("a refused completion left a connection")
	}
}

func TestMCPOAuthDisconnect(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, memberID, sharedID := mcpOAuthAgentFixture(t)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	agentRef, sharedRef := mcpoauth.AgentRef(agentID, "gateway"), mcpoauth.WorkspaceRef(sharedID)
	connectForTest(t, store, agentRef, mcpOAuthTestURL, ownerID, "access-token-one")

	// A connect left open in another tab before the disconnect.
	verifier, err := mcpoauth.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	staleState, err := store.CreatePendingAuthorization(context.Background(), mcpoauth.PendingAuthorization{
		WorkspaceID: testWorkspaceID, ServerRef: agentRef, ServerURL: mcpOAuthTestURL, UserID: ownerID,
		Resource: mcpOAuthTestURL, TokenEndpoint: "https://mcp.example.com/token",
		Client: mcpoauth.Client{ID: "client"}, RedirectURI: "https://app.example.test/oauth/mcp", Verifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}

	disconnect := base + "/gateway/connection"
	mcpOAuthCall(t, memberID, "DELETE", disconnect, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, ownerID, "DELETE", disconnect, nil).Want(http.StatusNoContent)
	var listed mcpOAuthListResponse
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if listed.Servers[0].Connection != nil {
		t.Fatalf("still connected after disconnect: %+v", listed.Servers[0].Connection)
	}
	// The stale tab cannot reconnect what was just disconnected.
	mcpOAuthCall(t, ownerID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": staleState, "code": "code"}).
		Want(http.StatusBadRequest)

	// From the workspace page each member disconnects only their own.
	connectForTest(t, store, sharedRef, "https://shared.example.com/mcp", ownerID, "owner-shared")
	connectForTest(t, store, sharedRef, "https://shared.example.com/mcp", memberID, "member-shared")
	mcpOAuthCall(t, memberID, "DELETE", "/api/workspaces/"+testWorkspaceID+"/mcp-oauth/"+sharedID+"/connection", nil).Want(http.StatusNoContent)
	if _, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, sharedRef, memberID); err == nil {
		t.Fatal("the member's connection survived their disconnect")
	}
	if _, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, sharedRef, ownerID); err != nil {
		t.Fatalf("another member's disconnect took the owner's connection: %v", err)
	}

	// An agent without an owner has no connection: a clean 404, not a 500.
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET owner_id = NULL WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	mcpOAuthCall(t, testUserID, "DELETE", disconnect, nil).Want(http.StatusNotFound)
}

// Through the real auth middleware: a local PAT authenticates whether it is
// sent as a Bearer header or placed in the auth cookie with a CSRF value
// computed from it, and the guard refuses it on every write either way. A
// session JWT, in the cookie or the header, is let through.
func TestMCPOAuthSessionGuardMatchesTheAuthMiddleware(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, _, sharedID := mcpOAuthAgentFixture(t)
	agentRef := mcpoauth.AgentRef(agentID, "gateway")
	connectForTest(t, store, agentRef, mcpOAuthTestURL, ownerID, "token-guarded")
	ctx := context.Background()

	pat := "mul_" + hex.EncodeToString(randomBytes(t, 20))
	var patID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO personal_access_token (user_id, name, token_hash, token_prefix)
		VALUES ($1, 'mcp oauth session guard', $2, $3) RETURNING id::text`,
		ownerID, auth.HashToken(pat), pat[:8]).Scan(&patID); err != nil {
		t.Fatalf("create PAT: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM personal_access_token WHERE id = $1`, patID) })
	session := mcpOAuthSession(t, ownerID, time.Hour, auth.JWTSecret())

	serve := middleware.Auth(testHandler.Queries, nil, nil, nil)(mcpOAuthRouter()).ServeHTTP
	send := func(how, token, method, path string, body any) *testutil.Response {
		t.Helper()
		req := newRequest(method, path, body)
		req.Header.Del("X-User-ID") // the middleware sets it from the credential
		switch how {
		case "cookie":
			req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: token})
			req.Header.Set(auth.CSRFHeaderName, tokenBoundCSRF(t, token))
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return testutil.Call(t, serve, req)
	}

	base := "/api/agents/" + agentID + "/mcp-oauth"
	wsBase := "/api/workspaces/" + testWorkspaceID + "/mcp-oauth"
	for _, how := range []string{"cookie", "bearer"} {
		// The middleware accepts the PAT this way: a read goes through.
		send(how, pat, "GET", base, nil).Want(http.StatusOK)
		for _, write := range []struct {
			method, path string
			body         any
		}{
			{"POST", base + "/gateway/connect", nil},
			{"DELETE", base + "/gateway/connection", nil},
			{"POST", wsBase + "/" + sharedID + "/connect", nil},
			{"DELETE", wsBase + "/" + sharedID + "/connection", nil},
			{"POST", "/api/mcp-oauth/complete", map[string]string{"state": "s", "code": "c"}},
		} {
			resp := send(how, pat, write.method, write.path, write.body).Want(http.StatusForbidden)
			if !strings.Contains(resp.Body.String(), "mcp_oauth_session_only") {
				t.Errorf("%s %s with the PAT as %s: %s", write.method, write.path, how, resp.Body.String())
			}
		}
	}
	if _, err := store.GetConnectionStatus(ctx, testWorkspaceID, agentRef, ownerID); err != nil {
		t.Fatalf("a PAT write got through: %v", err)
	}
	send("cookie", session, "DELETE", base+"/gateway/connection", nil).Want(http.StatusNoContent)
	connectForTest(t, store, agentRef, mcpOAuthTestURL, ownerID, "token-guarded")
	send("bearer", session, "DELETE", base+"/gateway/connection", nil).Want(http.StatusNoContent)
}

// Saving an entry that declares "auth" checks it as an OAuth entry while the
// flag is on — on the agent's mcp_config and on a workspace entry — and not
// while it is off.
func TestMCPOAuthEntriesAreValidatedOnSave(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID, _ := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	updateConfig := func(config string) int {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.UpdateAgent(w, withURLParam(newRequestAs(ownerID, http.MethodPut, "/api/agents/"+agentID,
			map[string]any{"mcp_config": json.RawMessage(config)}), "id", agentID))
		return w.Code
	}
	createEntry := func(name, config string) int {
		t.Helper()
		code, _, _ := createWorkspaceMcpServerViaAPI(t, map[string]any{"name": name, "config": json.RawMessage(config)}, nil)
		return code
	}
	bad := `{"url":"http://mcp.example.com/mcp","auth":{"type":"oauth"}}`

	withMCPOAuth(t, true)
	if code := updateConfig(`{"mcpServers":{"gateway":` + bad + `}}`); code != http.StatusBadRequest {
		t.Fatalf("agent config with an invalid OAuth entry: %d", code)
	}
	if code := updateConfig(`{"mcpServers":{"gateway":` + oauthEntry(mcpOAuthTestURL) + `}}`); code != http.StatusOK {
		t.Fatalf("agent config with a valid OAuth entry: %d", code)
	}
	if code := createEntry("bad-oauth-entry", bad); code != http.StatusBadRequest {
		t.Fatalf("workspace entry with an invalid OAuth entry: %d", code)
	}
	if code := createEntry("good-oauth-entry", oauthEntry(mcpOAuthTestURL)); code != http.StatusCreated {
		t.Fatalf("workspace entry with a valid OAuth entry: %d", code)
	}

	withMCPOAuth(t, false)
	if code := updateConfig(`{"mcpServers":{"gateway":` + bad + `}}`); code != http.StatusOK {
		t.Fatalf("flag off, agent config: %d, want upstream's 200", code)
	}
	if code := createEntry("bad-oauth-entry-off", bad); code != http.StatusCreated {
		t.Fatalf("flag off, workspace entry: %d, want upstream's 201", code)
	}
}

// Connections go with their entries: an agent entry removed or renamed in
// mcp_config, a workspace entry deleted, and a workspace entry unbound from
// the last agent of the owner's that used it.
func TestMCPOAuthConnectionsFollowTheirEntries(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	ctx := context.Background()
	connected := func(ref, userID string) bool {
		t.Helper()
		_, err := store.GetConnectionStatus(ctx, testWorkspaceID, ref, userID)
		return err == nil
	}

	agentID, ownerID, _ := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"keep":`+oauthEntry(mcpOAuthTestURL)+`,"drop":`+oauthEntry(mcpOAuthTestURL)+`}}`)
	keepRef, dropRef := mcpoauth.AgentRef(agentID, "keep"), mcpoauth.AgentRef(agentID, "drop")
	connectForTest(t, store, keepRef, mcpOAuthTestURL, ownerID, "keep-token")
	connectForTest(t, store, dropRef, mcpOAuthTestURL, ownerID, "drop-token")
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, withURLParam(newRequestAs(ownerID, http.MethodPut, "/api/agents/"+agentID,
		map[string]any{"mcp_config": json.RawMessage(`{"mcpServers":{"keep":` + oauthEntry(mcpOAuthTestURL) + `,"renamed":` + oauthEntry(mcpOAuthTestURL) + `}}`)}), "id", agentID))
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent: %d %s", w.Code, w.Body.String())
	}
	if !connected(keepRef, ownerID) || connected(dropRef, ownerID) {
		t.Fatalf("after the update: keep connected = %v, drop connected = %v", connected(keepRef, ownerID), connected(dropRef, ownerID))
	}

	// A workspace entry used by two agents of the owner: unbinding one keeps
	// the owner's connection, unbinding the last drops it.
	serverID, sharedRef := workspaceOAuthEntryForTest(t, "follow-gw", "https://shared.example.com/mcp")
	first := createHandlerTestAgent(t, "mcp oauth follow 1", nil)
	second := createHandlerTestAgent(t, "mcp oauth follow 2", nil)
	bindForTest(t, first, serverID)
	bindForTest(t, second, serverID)
	connectForTest(t, store, sharedRef, "https://shared.example.com/mcp", testUserID, "shared-token")
	unbind := func(agentID string) {
		t.Helper()
		req := withURLParams(newRequest(http.MethodDelete, "/api/agents/"+agentID+"/mcp-servers/"+serverID, nil), "id", agentID, "serverId", serverID)
		w := httptest.NewRecorder()
		testHandler.RemoveAgentMcpServer(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("RemoveAgentMcpServer: %d %s", w.Code, w.Body.String())
		}
	}
	unbind(first)
	if !connected(sharedRef, testUserID) {
		t.Fatal("unbinding one of two agents dropped the owner's connection")
	}
	unbind(second)
	if connected(sharedRef, testUserID) {
		t.Fatal("the connection survived its last binding")
	}

	// Deleting the entry takes every member's connection.
	connectForTest(t, store, sharedRef, "https://shared.example.com/mcp", testUserID, "shared-token")
	connectForTest(t, store, sharedRef, "https://shared.example.com/mcp", ownerID, "other-member-token")
	req := withURLParams(newRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/mcp-servers/"+serverID, nil), "id", testWorkspaceID, "serverId", serverID)
	w = httptest.NewRecorder()
	testHandler.DeleteWorkspaceMcpServer(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspaceMcpServer: %d %s", w.Code, w.Body.String())
	}
	if connected(sharedRef, testUserID) || connected(sharedRef, ownerID) {
		t.Fatal("a connection survived the deleted entry")
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSafeReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/ws/agents/1":         "/ws/agents/1",
		"":                     "",
		"https://evil.example": "",
		"//evil.example":       "",
		"/\\evil.example":      "",
		"relative":             "",
		"/a\r\nb":              "",
		"/\t/evil.example":     "",
		"/a\x00b":              "",
		"/a\x7fb":              "",
	} {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}
