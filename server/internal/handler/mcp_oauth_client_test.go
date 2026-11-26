// Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// preregisteredOAuthEntry is an OAuth entry naming client clientID of issuer.
func preregisteredOAuthEntry(url, clientID, issuer string) string {
	return `{"type":"http","url":"` + url + `","auth":{"type":"oauth","client_id":"` + clientID + `","issuer":"` + issuer + `"}}`
}

func cleanupMCPOAuthClient(t *testing.T, ref string) {
	t.Helper()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM mcp_oauth_client WHERE server_ref = $1`, ref)
	})
}

func clientSecretRows(t *testing.T, ref string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM mcp_oauth_client WHERE server_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The client secret of an agent's own entry is written by those who may edit
// the agent's MCP settings, in person; it is never returned, a list only
// says it is set; writing it takes the grant made with the old secret out of
// use.
func TestMCPOAuthAgentClientSecret(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	agentID, ownerID, memberID, _ := mcpOAuthAgentFixture(t)
	ref := mcpoauth.AgentRef(agentID, "gateway")
	cleanupMCPOAuthClient(t, ref)
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+preregisteredOAuthEntry(mcpOAuthTestURL, "pre-1", "https://auth.example.com")+`}}`)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	secretPath := base + "/gateway/client-secret"
	body := map[string]string{"client_secret": "sk-agent-secret-value"}

	mcpOAuthCall(t, memberID, "PUT", secretPath, body).Want(http.StatusForbidden)
	for name, credential := range mcpOAuthCredentials {
		req := newRequestAs(ownerID, "PUT", secretPath, body)
		credential(req)
		if resp := testutil.Call(t, mcpOAuthRouter(), req); resp.Code != http.StatusForbidden {
			t.Errorf("PUT with a %s: status %d", name, resp.Code)
		}
	}
	if clientSecretRows(t, ref) != 0 {
		t.Fatal("a refused write stored a secret")
	}
	mcpOAuthCall(t, ownerID, "PUT", secretPath, map[string]string{"client_secret": "bad\nsecret"}).Want(http.StatusBadRequest)
	mcpOAuthCall(t, ownerID, "PUT", secretPath, map[string]string{}).Want(http.StatusBadRequest)
	mcpOAuthCall(t, ownerID, "PUT", base+"/shared-gw/client-secret", body).Want(http.StatusConflict)
	mcpOAuthCall(t, ownerID, "PUT", base+"/missing/client-secret", body).Want(http.StatusNotFound)

	// The grant made with the client as it was goes out of use.
	connectForTest(t, store, ref, mcpOAuthTestURL, ownerID, "token-of-the-old-secret")
	mcpOAuthCall(t, ownerID, "PUT", secretPath, body).Want(http.StatusNoContent)
	status, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, ref, ownerID)
	if err != nil || status.Status != mcpoauth.StatusNeedsReconnect || status.LastError != mcpoauth.ReasonClientChanged {
		t.Fatalf("status after the new secret = %+v, %v", status, err)
	}

	resp := mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK)
	if strings.Contains(resp.Body.String(), "sk-agent-secret-value") || strings.Contains(resp.Body.String(), "pre-1") {
		t.Fatalf("the list carries the client: %s", resp.Body.String())
	}
	var listed mcpOAuthListResponse
	resp.JSON(&listed)
	var gateway *mcpOAuthServerResponse
	for i := range listed.Servers {
		if listed.Servers[i].Name == "gateway" {
			gateway = &listed.Servers[i]
		}
	}
	if gateway == nil || !gateway.Preregistered || !gateway.ClientSecretSet {
		t.Fatalf("gateway = %+v", gateway)
	}

	// Edited behind the save hooks to another issuer, the secret no longer
	// counts as set: it belongs to the old issuer.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+preregisteredOAuthEntry(mcpOAuthTestURL, "pre-1", "https://other-auth.example.com")+`}}`)
	mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK).JSON(&listed)
	for _, server := range listed.Servers {
		if server.Name == "gateway" && server.ClientSecretSet {
			t.Fatal("a secret set for another issuer is listed as set")
		}
	}
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+preregisteredOAuthEntry(mcpOAuthTestURL, "pre-1", "https://auth.example.com")+`}}`)

	// A workspace admin who is not the owner may manage it as well.
	mcpOAuthCall(t, testUserID, "DELETE", secretPath, nil).Want(http.StatusNoContent)
	if clientSecretRows(t, ref) != 0 {
		t.Fatal("the secret survived its delete")
	}
	mcpOAuthCall(t, ownerID, "DELETE", secretPath, nil).Want(http.StatusNoContent)

	// An entry that registers its client dynamically takes no secret.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+oauthEntry(mcpOAuthTestURL)+`}}`)
	if resp := mcpOAuthCall(t, ownerID, "PUT", secretPath, body).Want(http.StatusConflict); !strings.Contains(resp.Body.String(), "mcp_oauth_not_preregistered") {
		t.Fatalf("secret for a dynamic entry: %s", resp.Body.String())
	}
}

// The secret of a workspace library entry is written by the library's
// editors; any member sees only whether it is set.
func TestMCPOAuthWorkspaceClientSecret(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	_, ownerID, memberID, _ := mcpOAuthAgentFixture(t)
	serverID := createWorkspaceMcpServerForTest(t, "pre-gw", preregisteredOAuthEntry("https://pre.example.com/mcp", "pre-ws", "https://auth.example.com"))
	ref := mcpoauth.WorkspaceRef(serverID)
	cleanupMCPOAuthClient(t, ref)
	secretPath := "/api/workspaces/" + testWorkspaceID + "/mcp-oauth/" + serverID + "/client-secret"
	body := map[string]string{"client_secret": "sk-workspace-secret-value"}

	mcpOAuthCall(t, memberID, "PUT", secretPath, body).Want(http.StatusForbidden)
	mcpOAuthCall(t, ownerID, "PUT", secretPath, body).Want(http.StatusForbidden)
	mcpOAuthCall(t, testUserID, "PUT", secretPath, body).Want(http.StatusNoContent)
	if clientSecretRows(t, ref) != 1 {
		t.Fatal("secret not stored")
	}

	var listed workspaceMCPOAuthListResponse
	resp := mcpOAuthCall(t, memberID, "GET", "/api/workspaces/"+testWorkspaceID+"/mcp-oauth", nil).Want(http.StatusOK)
	if strings.Contains(resp.Body.String(), "sk-workspace-secret-value") {
		t.Fatalf("the list carries the secret: %s", resp.Body.String())
	}
	resp.JSON(&listed)
	found := false
	for _, server := range listed.Servers {
		if server.ServerID == serverID {
			found = server.Preregistered && server.ClientSecretSet
		}
	}
	if !found {
		t.Fatalf("list = %+v", listed.Servers)
	}

	mcpOAuthCall(t, memberID, "DELETE", secretPath, nil).Want(http.StatusForbidden)
	mcpOAuthCall(t, testUserID, "DELETE", secretPath, nil).Want(http.StatusNoContent)
	if clientSecretRows(t, ref) != 0 {
		t.Fatal("secret not deleted")
	}
}

// Saving an entry with another client takes the grants made with the old one
// out of use; going back to dynamic registration also drops the secret.
func TestMCPOAuthSavingAnotherClient(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	ctx := context.Background()
	entry := preregisteredOAuthEntry("https://pre.example.com/mcp", "pre-ws", "https://auth.example.com")
	serverID := createWorkspaceMcpServerForTest(t, "pre-gw-save", entry)
	ref := mcpoauth.WorkspaceRef(serverID)
	cleanupMCPOAuthClient(t, ref)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM mcp_oauth_connection WHERE server_ref = $1`, ref)
	})
	if err := store.SetClientSecret(ctx, testWorkspaceID, ref, testUserID,
		mcpoauth.ClientConfig{ID: "pre-ws", Issuer: "https://auth.example.com"}, mcpoauth.NewSecret("s")); err != nil {
		t.Fatal(err)
	}

	update := func(config string) {
		t.Helper()
		req := newRequest(http.MethodPut, "/api/workspaces/"+testWorkspaceID+"/mcp-servers/"+serverID,
			map[string]any{"config": json.RawMessage(config)})
		req = withURLParams(req, "id", testWorkspaceID, "serverId", serverID)
		w := httptest.NewRecorder()
		testHandler.UpdateWorkspaceMcpServer(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("update: %d %s", w.Code, w.Body.String())
		}
	}
	connected := func() string {
		t.Helper()
		status, err := store.GetConnectionStatus(ctx, testWorkspaceID, ref, testUserID)
		if err != nil {
			t.Fatal(err)
		}
		return status.Status
	}

	// Same client, other scope: the grant stays.
	connectForTest(t, store, ref, "https://pre.example.com/mcp", testUserID, "token-1")
	update(`{"type":"http","url":"https://pre.example.com/mcp","auth":{"type":"oauth","scope":"more","client_id":"pre-ws","issuer":"https://auth.example.com"}}`)
	if got := connected(); got != mcpoauth.StatusConnected {
		t.Fatalf("same client: %s", got)
	}

	// Same client, other method: the secret stays (it belongs to the client),
	// the grant made with the old method does not.
	update(`{"type":"http","url":"https://pre.example.com/mcp","auth":{"type":"oauth","client_id":"pre-ws","issuer":"https://auth.example.com","token_endpoint_auth_method":"client_secret_post"}}`)
	if got := connected(); got != mcpoauth.StatusNeedsReconnect {
		t.Fatalf("other method: %s", got)
	}
	if clientSecretRows(t, ref) != 1 {
		t.Fatal("a change of method dropped the secret")
	}

	// Another issuer: needs a reconnect, and the secret, set for the old
	// issuer, goes with it.
	connectForTest(t, store, ref, "https://pre.example.com/mcp", testUserID, "token-1b")
	update(preregisteredOAuthEntry("https://pre.example.com/mcp", "pre-ws", "https://other-auth.example.com"))
	if got := connected(); got != mcpoauth.StatusNeedsReconnect {
		t.Fatalf("another issuer: %s", got)
	}
	if clientSecretRows(t, ref) != 0 {
		t.Fatal("the old issuer's secret survived the change")
	}

	// Back to dynamic registration: needs a reconnect, and the secret goes.
	connectForTest(t, store, ref, "https://pre.example.com/mcp", testUserID, "token-2")
	update(oauthEntry("https://pre.example.com/mcp"))
	if got := connected(); got != mcpoauth.StatusNeedsReconnect {
		t.Fatalf("back to dynamic: %s", got)
	}
	if clientSecretRows(t, ref) != 0 {
		t.Fatal("a dynamic entry kept a client secret")
	}

	// A client secret in the entry itself is refused when saving.
	req := newRequest(http.MethodPut, "/api/workspaces/"+testWorkspaceID+"/mcp-servers/"+serverID,
		map[string]any{"config": json.RawMessage(`{"type":"http","url":"https://pre.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://auth.example.com","client_secret":"sk-in-entry"}}`)})
	req = withURLParams(req, "id", testWorkspaceID, "serverId", serverID)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspaceMcpServer(w, req)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "sk-in-entry") {
		t.Fatalf("secret in the entry: %d %s", w.Code, w.Body.String())
	}
}

// Stage 2 (M-295): the lists show only the hosts of an entry's manual
// endpoints, for the confirmation before connecting; a connect to manual
// endpoints on a non-public address is refused with its own code.
func TestMCPOAuthManualEndpoints(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	agentID, ownerID, _, _ := mcpOAuthAgentFixture(t)
	base := "/api/agents/" + agentID + "/mcp-oauth"
	manual := func(authorize, token string) string {
		return `{"mcpServers":{"gateway":{"type":"http","url":"` + mcpOAuthTestURL + `","auth":{"type":"oauth","client_id":"pre-1",` +
			`"issuer":"https://auth.example.com","authorization_endpoint":"` + authorize + `","token_endpoint":"` + token + `"}}}}`
	}

	setAgentMcpConfig(t, agentID, manual("https://login.example.com/oauth/authorize?tenant=t", "https://api.example.com:8443/oauth/token"))
	resp := mcpOAuthCall(t, ownerID, "GET", base, nil).Want(http.StatusOK)
	if strings.Contains(resp.Body.String(), "/oauth/") || strings.Contains(resp.Body.String(), "tenant") {
		t.Fatalf("the list carries endpoint URLs: %s", resp.Body.String())
	}
	var listed mcpOAuthListResponse
	resp.JSON(&listed)
	var gateway *mcpOAuthServerResponse
	for i := range listed.Servers {
		if listed.Servers[i].Name == "gateway" {
			gateway = &listed.Servers[i]
		}
	}
	if gateway == nil || gateway.AuthorizationHost != "login.example.com" || gateway.TokenHost != "api.example.com:8443" {
		t.Fatalf("gateway = %+v", gateway)
	}
	for _, server := range listed.Servers {
		if server.Name != "gateway" && (server.AuthorizationHost != "" || server.TokenHost != "") {
			t.Fatalf("%s shows hosts without manual endpoints: %+v", server.Name, server)
		}
	}

	// Saved directly: the save-time check would refuse these addresses.
	setAgentMcpConfig(t, agentID, manual("https://127.0.0.1:1/authorize", "https://127.0.0.1:1/token"))
	if resp := mcpOAuthCall(t, ownerID, "POST", base+"/gateway/connect", nil).Want(http.StatusBadRequest); !strings.Contains(resp.Body.String(), "mcp_oauth_endpoint_not_allowed") {
		t.Fatalf("connect to non-public manual endpoints: %s", resp.Body.String())
	}
}
