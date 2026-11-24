// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
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

func mcpOAuthCall(t *testing.T, userID, method, path string, body any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, mcpOAuthRouter(), newRequestAs(userID, method, path, body))
}

func asPAT(req *http.Request) *http.Request {
	req.Header.Set("X-Actor-Source", "cloud_pat")
	return req
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

	disconnect := base + "/" + server.ID + "/connection"
	mcpOAuthCall(t, memberID, "DELETE", disconnect, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, ownerID, "DELETE", disconnect, nil).Want(http.StatusNoContent)
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	if listed.Servers[0].Connection != nil {
		t.Fatalf("still connected after disconnect: %+v", listed.Servers[0].Connection)
	}
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
