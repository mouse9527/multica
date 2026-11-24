// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
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

func cleanupMCPOAuth(t *testing.T, agentID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM mcp_oauth_state WHERE server_id IN (SELECT id FROM mcp_oauth_server WHERE agent_id = $1)`, agentID)
		testPool.Exec(ctx, `DELETE FROM mcp_oauth_connection WHERE server_id IN (SELECT id FROM mcp_oauth_server WHERE agent_id = $1)`, agentID)
		testPool.Exec(ctx, `DELETE FROM mcp_oauth_server WHERE agent_id = $1`, agentID)
	})
}

func mcpOAuthRouter() http.HandlerFunc {
	r := chi.NewRouter()
	r.Route("/api/agents/{id}/mcp-oauth", testHandler.MountAgentMCPOAuth)
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

// Every write takes a signed-in session; no token of any kind, the local
// mul_ PAT included, may configure, connect, disconnect or complete.
func TestMCPOAuthWritesRefuseEveryToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	body := map[string]any{"name": "gateway", "url": "https://mcp.example.com/mcp"}
	var server mcpOAuthServerResponse
	mcpOAuthCall(t, ownerID, "POST", base, body).Want(http.StatusCreated).JSON(&server)

	writes := []struct {
		method, path string
		body         any
	}{
		{"POST", base, map[string]any{"name": "other", "url": "https://mcp.example.com/mcp"}},
		{"DELETE", base + "/" + server.ID, nil},
		{"POST", base + "/" + server.ID + "/connect", nil},
		{"DELETE", base + "/" + server.ID + "/connection", nil},
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
	// The server is still there: nothing above got through.
	var listed mcpOAuthListResponse
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if len(listed.Servers) != 1 {
		t.Fatalf("servers = %+v", listed.Servers)
	}

	// The guard is an allowlist: a JWT that is not this user's valid
	// session is refused like any token.
	for name, session := range map[string]string{
		"another user's session":     mcpOAuthSession(t, testUserID, time.Hour, auth.JWTSecret()),
		"an expired session":         mcpOAuthSession(t, ownerID, -time.Minute, auth.JWTSecret()),
		"a session signed elsewhere": mcpOAuthSession(t, ownerID, time.Hour, []byte("not-this-server's-secret-0123456789")),
		"no credential at all":       "",
	} {
		req := newRequestAs(ownerID, "POST", base, map[string]any{"name": "nope", "url": "https://mcp.example.com/mcp"})
		if session != "" {
			req.Header.Set("Authorization", "Bearer "+session)
		}
		if resp := testutil.Call(t, mcpOAuthRouter(), req); resp.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403: %s", name, resp.Code, resp.Body.String())
		}
	}

	// The user's own session JWT in the Authorization header (how the
	// desktop app signs in) is a session, not a token.
	mcpOAuthCall(t, ownerID, "POST", base, map[string]any{"name": "desktop", "url": "https://mcp.example.com/mcp"}).
		Want(http.StatusCreated)
}

func TestMCPOAuthRoutesAreOffWithoutTheFlag(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, false)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	mcpOAuthCall(t, ownerID, "GET", "/api/agents/"+agentID+"/mcp-oauth", nil).Want(http.StatusNotFound)
	mcpOAuthCall(t, ownerID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": "x", "code": "y"}).Want(http.StatusNotFound)
}

func TestMCPOAuthServerConfigurationPermissions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	body := map[string]any{
		"name": "gateway", "url": "https://mcp.example.com/mcp",
		"extra_headers": map[string]string{"x-gateway-key": "header-value-never-returned"},
	}

	// A plain member cannot configure or read it; a PAT cannot write.
	mcpOAuthCall(t, memberID, "POST", base, body).Want(http.StatusForbidden)
	mcpOAuthCall(t, memberID, "GET", base, nil).Want(http.StatusForbidden)
	testutil.Call(t, mcpOAuthRouter(), asPAT(newRequestAs(ownerID, "POST", base, body))).Want(http.StatusForbidden)

	// The owner can, and invalid input is a 400.
	mcpOAuthCall(t, ownerID, "POST", base, map[string]any{"name": "gateway", "url": "http://mcp.example.com/mcp"}).Want(http.StatusBadRequest)
	var created mcpOAuthServerResponse
	mcpOAuthCall(t, ownerID, "POST", base, body).Want(http.StatusCreated).JSON(&created)
	if created.Name != "gateway" || !created.HasExtraHeaders || created.Connection != nil {
		t.Fatalf("created = %+v", created)
	}
	mcpOAuthCall(t, ownerID, "POST", base, body).Want(http.StatusConflict)

	// Header values never come back.
	list := mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK)
	if strings.Contains(list.Body.String(), "header-value-never-returned") {
		t.Fatalf("list returned a header value: %s", list.Body.String())
	}
	var listed mcpOAuthListResponse
	list.JSON(&listed)
	if len(listed.Servers) != 1 || !listed.CanConnect {
		t.Fatalf("owner's list = %+v", listed)
	}
	// A workspace owner may configure, but connecting is the agent owner's.
	mcpOAuthCall(t, testUserID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if listed.CanConnect {
		t.Fatal("a workspace owner who is not the agent owner may connect")
	}

	// A server of another agent is not reachable through this one.
	otherAgent := dbfx.Agent(t, "mcp oauth other agent", handlerTestRuntimeID(t))
	cleanupMCPOAuth(t, otherAgent)
	var other mcpOAuthServerResponse
	mcpOAuthCall(t, testUserID, "POST", "/api/agents/"+otherAgent+"/mcp-oauth", body).Want(http.StatusCreated).JSON(&other)
	mcpOAuthCall(t, ownerID, "DELETE", base+"/"+other.ID, nil).Want(http.StatusNotFound)

	// Deleting: not a plain member; a workspace owner can.
	mcpOAuthCall(t, memberID, "DELETE", base+"/"+created.ID, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, testUserID, "DELETE", base+"/"+created.ID, nil).Want(http.StatusNoContent)
	mcpOAuthCall(t, testUserID, "DELETE", base+"/"+created.ID, nil).Want(http.StatusNotFound)
}

// Connecting is the agent owner's alone, in person, and the SSRF boundary
// holds: a server on a non-public address fails discovery.
func TestMCPOAuthConnectPermissions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	var server mcpOAuthServerResponse
	mcpOAuthCall(t, ownerID, "POST", base, map[string]any{"name": "local", "url": "https://127.0.0.1:1/mcp"}).
		Want(http.StatusCreated).JSON(&server)
	connect := base + "/" + server.ID + "/connect"

	mcpOAuthCall(t, testUserID, "POST", connect, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, memberID, "POST", connect, nil).Want(http.StatusForbidden)
	testutil.Call(t, mcpOAuthRouter(), asPAT(newRequestAs(ownerID, "POST", connect, nil))).Want(http.StatusForbidden)

	resp := mcpOAuthCall(t, ownerID, "POST", connect, map[string]string{"return_to": "/ws/agents"}).Want(http.StatusBadGateway)
	if !strings.Contains(resp.Body.String(), "mcp_oauth_upstream") {
		t.Fatalf("connect to a non-public server: %s", resp.Body.String())
	}

	// Without an app URL there is no redirect URI to register.
	testHandler.cfg.AppURL = ""
	mcpOAuthCall(t, ownerID, "POST", connect, nil).Want(http.StatusServiceUnavailable)
}

// Completing checks the user against the state and the agent's current owner
// before anything is exchanged.
func TestMCPOAuthCompleteChecksWhoFinishes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	var server mcpOAuthServerResponse
	mcpOAuthCall(t, ownerID, "POST", "/api/agents/"+agentID+"/mcp-oauth", map[string]any{"name": "gateway", "url": "https://mcp.example.com/mcp"}).
		Want(http.StatusCreated).JSON(&server)
	verifier, err := mcpoauth.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	start := func() string {
		t.Helper()
		state, err := store.CreatePendingAuthorization(context.Background(), mcpoauth.PendingAuthorization{
			WorkspaceID: testWorkspaceID, ServerID: server.ID, UserID: ownerID,
			Resource: "https://mcp.example.com/mcp", TokenEndpoint: "https://mcp.example.com/token",
			Client: mcpoauth.Client{ID: "client"}, RedirectURI: "https://app.example.test/oauth/mcp", Verifier: verifier,
		})
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	complete := func(userID, state, code string) *testutil.Response {
		t.Helper()
		return mcpOAuthCall(t, userID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": state, "code": code})
	}

	// Someone else's session cannot finish the owner's connect.
	complete(memberID, start(), "code").Want(http.StatusBadRequest)
	complete(ownerID, "not-a-state", "code").Want(http.StatusBadRequest)
	testutil.Call(t, mcpOAuthRouter(), asPAT(newRequestAs(ownerID, "POST", "/api/mcp-oauth/complete",
		map[string]string{"state": start(), "code": "code"}))).Want(http.StatusForbidden)

	// Declined at the authorization server: the state is used up.
	declined := start()
	mcpOAuthCall(t, ownerID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": declined, "error": "access_denied"}).Want(http.StatusBadRequest)
	complete(ownerID, declined, "code").Want(http.StatusBadRequest)

	// The agent changed owner between start and finish: refused before the
	// code is exchanged (the token endpoint here does not exist, so an
	// exchange would have been a 502).
	state := start()
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET owner_id = $1 WHERE id = $2`, memberID, agentID); err != nil {
		t.Fatal(err)
	}
	complete(ownerID, state, "code").Want(http.StatusForbidden)

	if _, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, server.ID, ownerID); err == nil {
		t.Fatal("a refused completion left a connection")
	}
}

func TestMCPOAuthDisconnect(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	var server mcpOAuthServerResponse
	mcpOAuthCall(t, ownerID, "POST", base, map[string]any{"name": "gateway", "url": "https://mcp.example.com/mcp"}).
		Want(http.StatusCreated).JSON(&server)
	if err := store.SaveGrant(context.Background(), mcpoauth.Grant{
		WorkspaceID: testWorkspaceID, ServerID: server.ID, UserID: ownerID,
		Resource: "https://mcp.example.com/mcp", TokenEndpoint: "https://mcp.example.com/token",
		Client: mcpoauth.Client{ID: "client"}, AccessToken: mcpoauth.NewSecret("access-token-never-returned"), ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	list := mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK)
	if strings.Contains(list.Body.String(), "access-token-never-returned") {
		t.Fatalf("list returned the token: %s", list.Body.String())
	}
	var listed mcpOAuthListResponse
	list.JSON(&listed)
	if len(listed.Servers) != 1 || listed.Servers[0].Connection == nil || listed.Servers[0].Connection.Status != mcpoauth.StatusConnected {
		t.Fatalf("list = %+v", listed)
	}

	// A connect left open in another tab before the disconnect.
	verifier, err := mcpoauth.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	staleState, err := store.CreatePendingAuthorization(context.Background(), mcpoauth.PendingAuthorization{
		WorkspaceID: testWorkspaceID, ServerID: server.ID, UserID: ownerID,
		Resource: "https://mcp.example.com/mcp", TokenEndpoint: "https://mcp.example.com/token",
		Client: mcpoauth.Client{ID: "client"}, RedirectURI: "https://app.example.test/oauth/mcp", Verifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}

	disconnect := base + "/" + server.ID + "/connection"
	mcpOAuthCall(t, memberID, "DELETE", disconnect, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, ownerID, "DELETE", disconnect, nil).Want(http.StatusNoContent)
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if listed.Servers[0].Connection != nil {
		t.Fatalf("still connected after disconnect: %+v", listed.Servers[0].Connection)
	}
	// The stale tab cannot reconnect what was just disconnected.
	mcpOAuthCall(t, ownerID, "POST", "/api/mcp-oauth/complete", map[string]string{"state": staleState, "code": "code"}).
		Want(http.StatusBadRequest)

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
	withMCPOAuth(t, true)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	cleanupMCPOAuth(t, agentID)
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
	var server mcpOAuthServerResponse
	send("cookie", session, "POST", base, map[string]any{"name": "gateway", "url": "https://mcp.example.com/mcp"}).
		Want(http.StatusCreated).JSON(&server)
	send("bearer", session, "POST", base, map[string]any{"name": "desktop", "url": "https://mcp.example.com/mcp"}).
		Want(http.StatusCreated)

	for _, how := range []string{"cookie", "bearer"} {
		// The middleware accepts the PAT this way: a read goes through.
		send(how, pat, "GET", base, nil).Want(http.StatusOK)
		for _, write := range []struct {
			method, path string
			body         any
		}{
			{"POST", base, map[string]any{"name": "by-pat", "url": "https://mcp.example.com/mcp"}},
			{"POST", base + "/" + server.ID + "/connect", nil},
			{"DELETE", base + "/" + server.ID + "/connection", nil},
			{"DELETE", base + "/" + server.ID, nil},
			{"POST", "/api/mcp-oauth/complete", map[string]string{"state": "s", "code": "c"}},
		} {
			resp := send(how, pat, write.method, write.path, write.body).Want(http.StatusForbidden)
			if !strings.Contains(resp.Body.String(), "mcp_oauth_session_only") {
				t.Errorf("%s %s with the PAT as %s: %s", write.method, write.path, how, resp.Body.String())
			}
		}
	}
	var listed mcpOAuthListResponse
	send("cookie", session, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if len(listed.Servers) != 2 {
		t.Fatalf("servers after the PAT writes = %+v, want the two the session made", listed.Servers)
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
	} {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}
