// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

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

func mcpOAuthCredentialCall(t *testing.T, taskID, serverID string) *testutil.Response {
	t.Helper()
	return mcpOAuthCredentialCallQuery(t, taskID, serverID, "")
}

func mcpOAuthCredentialCallQuery(t *testing.T, taskID, serverID, query string) *testutil.Response {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/daemon/tasks/{id}/mcp-oauth/{serverId}/credential", testHandler.ResolveMCPOAuthCredential)
	path := "/api/daemon/tasks/" + taskID + "/mcp-oauth/" + serverID + "/credential"
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

// The daemon's proxy gets the owner's current token for a server of the
// running task's agent, and nothing else.
func TestResolveMCPOAuthCredential(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	_, agentID, taskID := mcpOAuthClaimFixture(t, "mcp-oauth-credential", "")
	gateway := connectForClaimTest(t, store, agentID, "gateway", "credential-token", map[string]string{"x-mcp-servers": "browser"})
	pending := connectForClaimTest(t, store, agentID, "pending", "", nil)
	dead := connectForClaimTest(t, store, agentID, "dead", "credential-token-dead", nil)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE mcp_oauth_connection SET status = 'needs_reconnect', access_token_ciphertext = NULL,
			refresh_token_ciphertext = NULL, expires_at = NULL
		WHERE server_id = $1`, dead.ID); err != nil {
		t.Fatal(err)
	}

	// A queued task is not running yet.
	mcpOAuthCredentialCall(t, taskID, gateway.ID).Want(http.StatusConflict)

	setTaskStatus(t, taskID, "running")
	var got mcpOAuthCredentialResponse
	resp := mcpOAuthCredentialCall(t, taskID, gateway.ID).Want(http.StatusOK)
	resp.JSON(&got)
	if got.Headers["Authorization"] != "Bearer credential-token" || got.Headers["x-mcp-servers"] != "browser" ||
		got.ExpiresAt == nil || time.Until(*got.ExpiresAt) < 50*time.Minute {
		t.Fatalf("credential = %+v", got)
	}
	if resp.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header().Get("Cache-Control"))
	}

	notConnected := mcpOAuthCredentialCall(t, taskID, pending.ID).Want(http.StatusNotFound)
	if !strings.Contains(notConnected.Body.String(), "mcp_oauth_not_connected") {
		t.Fatalf("not connected: %s", notConnected.Body.String())
	}
	reconnect := mcpOAuthCredentialCall(t, taskID, dead.ID).Want(http.StatusConflict)
	if !strings.Contains(reconnect.Body.String(), "mcp_oauth_needs_reconnect") {
		t.Fatalf("needs reconnect: %s", reconnect.Body.String())
	}

	// A server of another agent is not served through this task.
	otherAgent := dbfx.Agent(t, "mcp oauth credential other agent", handlerTestRuntimeID(t))
	cleanupMCPOAuth(t, otherAgent)
	other := connectForClaimTest(t, store, otherAgent, "elsewhere", "other-agent-token", nil)
	if body := mcpOAuthCredentialCall(t, taskID, other.ID).Want(http.StatusNotFound).Body.String(); strings.Contains(body, "other-agent-token") {
		t.Fatal("another agent's token was served")
	}

	// A malformed rejected-token hash is refused; a hash of a token that is
	// not the stored one changes nothing.
	mcpOAuthCredentialCallQuery(t, taskID, gateway.ID, "rejected=not-a-hash").Want(http.StatusBadRequest)
	var unchanged mcpOAuthCredentialResponse
	mcpOAuthCredentialCallQuery(t, taskID, gateway.ID, "rejected="+mcpoauth.TokenHash("some-other-token")).Want(http.StatusOK).JSON(&unchanged)
	if unchanged.Headers["Authorization"] != "Bearer credential-token" {
		t.Fatalf("credential = %+v", unchanged)
	}

	// With the flag off the route does not exist.
	withMCPOAuth(t, false)
	mcpOAuthCredentialCall(t, taskID, gateway.ID).Want(http.StatusNotFound)

	// Once the task has finished, no token is served for it. (The task's
	// state is checked before any secret is opened, so the new key the
	// flag switch set does not matter here.)
	withMCPOAuth(t, true)
	setTaskStatus(t, taskID, "completed")
	mcpOAuthCredentialCall(t, taskID, gateway.ID).Want(http.StatusConflict)
}
