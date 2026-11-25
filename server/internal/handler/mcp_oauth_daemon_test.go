// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package handler

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// mcpOAuthCredentialCall fetches a credential the way the daemon's proxy
// does, with the handle the claim gave it (mcpoauth.ProxyHandle).
func mcpOAuthCredentialCall(t *testing.T, taskID, handle string) *testutil.Response {
	t.Helper()
	return mcpOAuthCredentialCallQuery(t, taskID, handle, "")
}

func mcpOAuthCredentialCallQuery(t *testing.T, taskID, handle, query string) *testutil.Response {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/daemon/tasks/{id}/mcp-oauth/{serverId}/credential", testHandler.ResolveMCPOAuthCredential)
	path := "/api/daemon/tasks/" + taskID + "/mcp-oauth/" + url.PathEscape(handle) + "/credential"
	if query != "" {
		path += "?" + query
	}
	req := newOwnerDaemonRequest(http.MethodGet, path, nil)
	return testutil.Call(t, r.ServeHTTP, req)
}

func setTaskStatus(t *testing.T, taskID, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_task_queue SET status = $1::text,
			dispatched_at = COALESCE(dispatched_at, now()),
			started_at = COALESCE(started_at, now()),
			completed_at = CASE WHEN $1::text IN ('completed', 'failed', 'cancelled') THEN now() ELSE NULL END
		WHERE id = $2`, status, taskID); err != nil {
		t.Fatal(err)
	}
}

// The daemon's proxy gets the owner's current token for an OAuth entry the
// running task's agent runs with — its own or a bound workspace entry — and
// nothing else.
func TestResolveMCPOAuthCredential(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	_, agentID, taskID := mcpOAuthClaimFixture(t, "mcp-oauth-credential", `{"mcpServers":{`+
		`"gateway":`+oauthEntry("https://mcp.example.com/gateway")+`,`+
		`"pending":`+oauthEntry("https://mcp.example.com/pending")+`,`+
		`"dead":`+oauthEntry("https://mcp.example.com/dead")+`,`+
		`"plain":{"url":"https://plain.example.com/mcp"}}}`)
	gatewayRef, deadRef := mcpoauth.AgentRef(agentID, "gateway"), mcpoauth.AgentRef(agentID, "dead")
	gateway := mcpoauth.ProxyHandle(gatewayRef, "https://mcp.example.com/gateway")
	pending := mcpoauth.ProxyHandle(mcpoauth.AgentRef(agentID, "pending"), "https://mcp.example.com/pending")
	dead := mcpoauth.ProxyHandle(deadRef, "https://mcp.example.com/dead")
	connectForTest(t, store, gatewayRef, "https://mcp.example.com/gateway", testUserID, "credential-token")
	connectForTest(t, store, deadRef, "https://mcp.example.com/dead", testUserID, "credential-token-dead")
	markNeedsReconnect(t, deadRef)
	sharedID, sharedRef := workspaceOAuthEntryForTest(t, "credential-shared", "https://shared.example.com/mcp")
	bindForTest(t, agentID, sharedID)
	connectForTest(t, store, sharedRef, "https://shared.example.com/mcp", testUserID, "credential-token-shared")
	shared := mcpoauth.ProxyHandle(sharedRef, "https://shared.example.com/mcp")

	// A queued task is not running yet.
	mcpOAuthCredentialCall(t, taskID, gateway).Want(http.StatusConflict)

	setTaskStatus(t, taskID, "running")
	var got mcpOAuthCredentialResponse
	resp := mcpOAuthCredentialCall(t, taskID, gateway).Want(http.StatusOK)
	resp.JSON(&got)
	if got.Headers["Authorization"] != "Bearer credential-token" || len(got.Headers) != 1 ||
		got.URL != "https://mcp.example.com/gateway" || got.ExpiresAt == nil || time.Until(*got.ExpiresAt) < 50*time.Minute {
		t.Fatalf("credential = %+v", got)
	}
	if resp.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header().Get("Cache-Control"))
	}
	mcpOAuthCredentialCall(t, taskID, shared).Want(http.StatusOK).JSON(&got)
	if got.Headers["Authorization"] != "Bearer credential-token-shared" || got.URL != "https://shared.example.com/mcp" {
		t.Fatalf("shared credential = %+v", got)
	}

	notConnected := mcpOAuthCredentialCall(t, taskID, pending).Want(http.StatusNotFound)
	if !strings.Contains(notConnected.Body.String(), "mcp_oauth_not_connected") {
		t.Fatalf("not connected: %s", notConnected.Body.String())
	}
	reconnect := mcpOAuthCredentialCall(t, taskID, dead).Want(http.StatusConflict)
	if !strings.Contains(reconnect.Body.String(), "mcp_oauth_needs_reconnect") {
		t.Fatalf("needs reconnect: %s", reconnect.Body.String())
	}
	// Not an OAuth entry, a bare ref without its URL, not a handle, not an
	// entry at all.
	for _, handle := range []string{
		mcpoauth.ProxyHandle(mcpoauth.AgentRef(agentID, "plain"), "https://plain.example.com/mcp"),
		gatewayRef, "gateway", gatewayRef + "~not-a-digest",
		mcpoauth.ProxyHandle(mcpoauth.AgentRef(agentID, "missing"), "https://mcp.example.com/missing"),
	} {
		mcpOAuthCredentialCall(t, taskID, handle).Want(http.StatusNotFound)
	}

	// Another agent's entry, connected by the same owner, is not served
	// through this task; neither is a workspace entry not bound to it.
	otherAgent := dbfx.Agent(t, "mcp oauth credential other agent", handlerTestRuntimeID(t))
	cleanupMCPOAuth(t, otherAgent)
	setAgentMcpConfig(t, otherAgent, `{"mcpServers":{"elsewhere":`+oauthEntry("https://mcp.example.com/elsewhere")+`}}`)
	otherRef := mcpoauth.AgentRef(otherAgent, "elsewhere")
	connectForTest(t, store, otherRef, "https://mcp.example.com/elsewhere", testUserID, "other-agent-token")
	_, unboundRef := workspaceOAuthEntryForTest(t, "credential-unbound", "https://unbound.example.com/mcp")
	connectForTest(t, store, unboundRef, "https://unbound.example.com/mcp", testUserID, "unbound-token")
	for _, handle := range []string{
		mcpoauth.ProxyHandle(otherRef, "https://mcp.example.com/elsewhere"),
		mcpoauth.ProxyHandle(unboundRef, "https://unbound.example.com/mcp"),
	} {
		if body := mcpOAuthCredentialCall(t, taskID, handle).Want(http.StatusNotFound).Body.String(); strings.Contains(body, "Bearer") {
			t.Fatalf("%s: a token was served: %s", handle, body)
		}
	}

	// A malformed rejected-token hash is refused; a hash of a token that is
	// not the stored one changes nothing.
	mcpOAuthCredentialCallQuery(t, taskID, gateway, "rejected=not-a-hash").Want(http.StatusBadRequest)
	var unchanged mcpOAuthCredentialResponse
	mcpOAuthCredentialCallQuery(t, taskID, gateway, "rejected="+mcpoauth.TokenHash("some-other-token")).Want(http.StatusOK).JSON(&unchanged)
	if unchanged.Headers["Authorization"] != "Bearer credential-token" {
		t.Fatalf("credential = %+v", unchanged)
	}

	// With the flag off the route does not exist.
	withMCPOAuth(t, false)
	mcpOAuthCredentialCall(t, taskID, shared).Want(http.StatusNotFound)

	// Once the task has finished, no token is served for it. (The task's
	// state is checked before any secret is opened, so the new key the
	// flag switch set does not matter here.)
	withMCPOAuth(t, true)
	setTaskStatus(t, taskID, "completed")
	mcpOAuthCredentialCall(t, taskID, shared).Want(http.StatusConflict)
}

// The proxy a claim started for URL A keeps sending to A for the rest of the
// run. The entry is pointed at B and reconnected for B while the task runs:
// the old proxy's next fetch gets no token at all, so B's token never goes to
// A, and the connection made for B is left as it is. A proxy started for B
// (the next task) gets B's token. Covers an agent's own entry and a
// workspace entry.
func TestResolveMCPOAuthCredentialRefusesAProxyForTheOldURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	const urlA, urlB = "https://a.example.com/mcp", "https://b.example.com/mcp"
	runtimeID, agentID, taskID := mcpOAuthClaimFixture(t, "mcp-oauth-credential-moved", `{"mcpServers":{"gateway":`+oauthEntry(urlA)+`}}`)
	agentRef := mcpoauth.AgentRef(agentID, "gateway")
	sharedID, sharedRef := workspaceOAuthEntryForTest(t, "moved-shared", urlA)
	bindForTest(t, agentID, sharedID)
	connectForTest(t, store, agentRef, urlA, testUserID, "token-for-a")
	connectForTest(t, store, sharedRef, urlA, testUserID, "shared-token-for-a")

	// The claim starts the proxies for A.
	agent := claimTaskAgentForTest(t, runtimeID, protocol.DaemonCapabilityMCPOAuthProxyV1)
	handles := map[string]string{}
	for _, proxy := range agent.MCPOAuthProxy {
		if proxy.URL != urlA {
			t.Fatalf("proxy %+v, want URL A", proxy)
		}
		handles[proxy.Name] = proxy.ServerID
	}
	if len(handles) != 2 {
		t.Fatalf("proxies = %+v", agent.MCPOAuthProxy)
	}
	setTaskStatus(t, taskID, "running")
	for name, handle := range handles {
		var got mcpOAuthCredentialResponse
		mcpOAuthCredentialCall(t, taskID, handle).Want(http.StatusOK).JSON(&got)
		if got.URL != urlA {
			t.Fatalf("%s before the move: %+v", name, got)
		}
	}

	// Both entries move to B and are reconnected for B mid-task.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+oauthEntry(urlB)+`}}`)
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace_mcp_server SET config = $1 WHERE id = $2`, []byte(oauthEntry(urlB)), sharedID); err != nil {
		t.Fatal(err)
	}
	connectForTest(t, store, agentRef, urlB, testUserID, "token-for-b")
	connectForTest(t, store, sharedRef, urlB, testUserID, "shared-token-for-b")

	for name, handle := range handles {
		body := mcpOAuthCredentialCall(t, taskID, handle).Want(http.StatusConflict).Body.String()
		if !strings.Contains(body, "mcp_oauth_entry_changed") || strings.Contains(body, "for-b") {
			t.Fatalf("%s: the old proxy got %s", name, body)
		}
	}
	for _, ref := range []string{agentRef, sharedRef} {
		status, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, ref, testUserID)
		if err != nil || status.Status != mcpoauth.StatusConnected || status.ServerURL != urlB {
			t.Fatalf("%s: the connection for B was disturbed: %+v, %v", ref, status, err)
		}
	}
	// A proxy for B gets B's token.
	var got mcpOAuthCredentialResponse
	mcpOAuthCredentialCall(t, taskID, mcpoauth.ProxyHandle(agentRef, urlB)).Want(http.StatusOK).JSON(&got)
	if got.Headers["Authorization"] != "Bearer token-for-b" || got.URL != urlB {
		t.Fatalf("proxy for B: %+v", got)
	}
}
