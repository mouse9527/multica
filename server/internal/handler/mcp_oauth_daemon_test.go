// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func mcpOAuthCredentialCall(t *testing.T, taskID, ref string) *testutil.Response {
	t.Helper()
	return mcpOAuthCredentialCallQuery(t, taskID, ref, "")
}

func mcpOAuthCredentialCallQuery(t *testing.T, taskID, ref, query string) *testutil.Response {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/daemon/tasks/{id}/mcp-oauth/{serverId}/credential", testHandler.ResolveMCPOAuthCredential)
	path := "/api/daemon/tasks/" + taskID + "/mcp-oauth/" + ref + "/credential"
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
	gateway, pending, dead := mcpoauth.AgentRef(agentID, "gateway"), mcpoauth.AgentRef(agentID, "pending"), mcpoauth.AgentRef(agentID, "dead")
	connectForTest(t, store, gateway, "https://mcp.example.com/gateway", testUserID, "credential-token")
	connectForTest(t, store, dead, "https://mcp.example.com/dead", testUserID, "credential-token-dead")
	markNeedsReconnect(t, dead)
	sharedID, shared := workspaceOAuthEntryForTest(t, "credential-shared", "https://shared.example.com/mcp")
	bindForTest(t, agentID, sharedID)
	connectForTest(t, store, shared, "https://shared.example.com/mcp", testUserID, "credential-token-shared")

	// A queued task is not running yet.
	mcpOAuthCredentialCall(t, taskID, gateway).Want(http.StatusConflict)

	setTaskStatus(t, taskID, "running")
	var got mcpOAuthCredentialResponse
	resp := mcpOAuthCredentialCall(t, taskID, gateway).Want(http.StatusOK)
	resp.JSON(&got)
	if got.Headers["Authorization"] != "Bearer credential-token" || len(got.Headers) != 1 ||
		got.ExpiresAt == nil || time.Until(*got.ExpiresAt) < 50*time.Minute {
		t.Fatalf("credential = %+v", got)
	}
	if resp.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header().Get("Cache-Control"))
	}
	mcpOAuthCredentialCall(t, taskID, shared).Want(http.StatusOK).JSON(&got)
	if got.Headers["Authorization"] != "Bearer credential-token-shared" {
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
	// Not an OAuth entry, not a ref, not an entry at all.
	for _, ref := range []string{mcpoauth.AgentRef(agentID, "plain"), "gateway", mcpoauth.AgentRef(agentID, "missing")} {
		mcpOAuthCredentialCall(t, taskID, ref).Want(http.StatusNotFound)
	}

	// Another agent's entry, connected by the same owner, is not served
	// through this task; neither is a workspace entry not bound to it.
	otherAgent := dbfx.Agent(t, "mcp oauth credential other agent", handlerTestRuntimeID(t))
	cleanupMCPOAuth(t, otherAgent)
	setAgentMcpConfig(t, otherAgent, `{"mcpServers":{"elsewhere":`+oauthEntry("https://mcp.example.com/elsewhere")+`}}`)
	other := mcpoauth.AgentRef(otherAgent, "elsewhere")
	connectForTest(t, store, other, "https://mcp.example.com/elsewhere", testUserID, "other-agent-token")
	_, unbound := workspaceOAuthEntryForTest(t, "credential-unbound", "https://unbound.example.com/mcp")
	connectForTest(t, store, unbound, "https://unbound.example.com/mcp", testUserID, "unbound-token")
	for _, ref := range []string{other, unbound} {
		if body := mcpOAuthCredentialCall(t, taskID, ref).Want(http.StatusNotFound).Body.String(); strings.Contains(body, "Bearer") {
			t.Fatalf("%s: a token was served: %s", ref, body)
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

	// The entry moved while the task ran: no token for the new URL.
	setAgentMcpConfig(t, agentID, `{"mcpServers":{"gateway":`+oauthEntry("https://attacker.example.com/mcp")+`}}`)
	if body := mcpOAuthCredentialCall(t, taskID, gateway).Want(http.StatusConflict).Body.String(); !strings.Contains(body, "mcp_oauth_needs_reconnect") {
		t.Fatalf("moved entry: %s", body)
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
