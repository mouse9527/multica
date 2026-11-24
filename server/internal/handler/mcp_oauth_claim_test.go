// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// mcpOAuthClaimFixture is an agent owned by testUserID with one queued task
// on its own runtime, and its saved mcp_config.
func mcpOAuthClaimFixture(t *testing.T, name, agentMcpConfig string) (runtimeID, agentID, taskID string) {
	t.Helper()
	ctx := context.Background()
	runtimeID = createClaimReclaimRuntime(t, ctx, name+" runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, name+" agent")
	cleanupMCPOAuth(t, agentID)
	if agentMcpConfig != "" {
		if _, err := testPool.Exec(ctx, `UPDATE agent SET mcp_config = $1 WHERE id = $2`, []byte(agentMcpConfig), agentID); err != nil {
			t.Fatal(err)
		}
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0) RETURNING id`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	return runtimeID, agentID, taskID
}

func claimTaskAgentForTest(t *testing.T, runtimeID string) *TaskAgentData {
	t.Helper()
	w := httptest.NewRecorder()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *AgentTaskResponse `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Task == nil || resp.Task.Agent == nil {
		t.Fatalf("no task agent in the claim: %s", w.Body.String())
	}
	return resp.Task.Agent
}

func connectForClaimTest(t *testing.T, store *mcpoauth.Store, agentID, name, accessToken string, headers map[string]string) mcpoauth.Server {
	t.Helper()
	ctx := context.Background()
	server, err := store.CreateServer(ctx, mcpoauth.ServerInput{
		WorkspaceID: testWorkspaceID, AgentID: agentID, CreatedBy: testUserID,
		Name: name, URL: "https://mcp.example.com/" + name, ExtraHeaders: headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if accessToken == "" {
		return server
	}
	if err := store.SaveGrant(ctx, mcpoauth.Grant{
		WorkspaceID: testWorkspaceID, ServerID: server.ID, UserID: testUserID,
		Resource: server.URL, TokenEndpoint: "https://mcp.example.com/token",
		Client: mcpoauth.Client{ID: "client"}, AccessToken: mcpoauth.NewSecret(accessToken),
		RefreshToken: mcpoauth.NewSecret("refresh-" + accessToken), ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	return server
}

// With the flag off the claim is upstream's: nothing from the OAuth tables,
// no notices, even for a connected server.
func TestClaimWithoutMCPOAuthFlagIsUpstream(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, false)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-off", `{"mcpServers":{"own":{"url":"https://own.example"}}}`)
	connectForClaimTest(t, store, agentID, "gateway", "claim-token-off", nil)

	agent := claimTaskAgentForTest(t, runtimeID)
	servers := decodeServers(t, agent.McpConfig)
	if len(servers) != 1 || servers["own"] == nil || len(agent.MCPOAuthNotices) != 0 {
		t.Fatalf("servers = %v, notices = %v; want the agent's own config only", serverNames(servers), agent.MCPOAuthNotices)
	}
}

// A connected server is mounted with the owner's token and its extra
// headers, replacing a same-named entry; servers that cannot be mounted are
// left out and named in a notice; the token is nowhere in the task row.
func TestClaimMountsConnectedMCPOAuthServers(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, taskID := mcpOAuthClaimFixture(t, "mcp-oauth-claim-on",
		`{"mcpServers":{"own":{"url":"https://own.example"},"gateway":{"url":"https://stale.example"}}}`)
	gateway := connectForClaimTest(t, store, agentID, "gateway", "claim-token-live", map[string]string{"x-mcp-servers": "browser,phone"})
	connectForClaimTest(t, store, agentID, "pending", "", nil)
	dead := connectForClaimTest(t, store, agentID, "dead", "claim-token-dead", nil)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE mcp_oauth_connection SET status = 'needs_reconnect', access_token_ciphertext = NULL,
			refresh_token_ciphertext = NULL, expires_at = NULL
		WHERE server_id = $1`, dead.ID); err != nil {
		t.Fatal(err)
	}

	agent := claimTaskAgentForTest(t, runtimeID)
	servers := decodeServers(t, agent.McpConfig)
	if len(servers) != 2 || servers["own"] == nil || servers["gateway"] == nil {
		t.Fatalf("servers = %v, want own and gateway", serverNames(servers))
	}
	entry, _ := servers["gateway"].(map[string]any)
	headers, _ := entry["headers"].(map[string]any)
	if entry["type"] != "http" || entry["url"] != gateway.URL ||
		headers["Authorization"] != "Bearer claim-token-live" || headers["x-mcp-servers"] != "browser,phone" {
		t.Fatalf("gateway entry = %v", entry)
	}

	notices := strings.Join(agent.MCPOAuthNotices, "\n")
	if len(agent.MCPOAuthNotices) != 2 ||
		!strings.Contains(notices, `"pending" was not mounted: it is not connected yet`) ||
		!strings.Contains(notices, `"dead" was not mounted: its authorization expired or was revoked`) {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
	if strings.Contains(notices, "claim-token") {
		t.Fatal("a notice carries a token")
	}

	var row string
	if err := testPool.QueryRow(context.Background(), `SELECT t::text FROM agent_task_queue t WHERE id = $1`, taskID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, "claim-token-live") {
		t.Fatal("the token was written to agent_task_queue")
	}
}

// An agent without an owner has no grant to use: its servers are left out
// with a notice. (A private runtime refuses an ownerless agent before this
// point, so the hook is called directly with the agent row.)
func TestClaimMCPOAuthWithoutOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	_, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-ownerless", "")
	connectForClaimTest(t, store, agentID, "gateway", "claim-token-ownerless", nil)
	agent, err := testHandler.Queries.GetAgent(context.Background(), util.MustParseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	agent.OwnerID = pgtype.UUID{}
	mcpConfig, notices := testHandler.applyMCPOAuthServers(context.Background(), &db.AgentTaskQueue{}, agent, nil)
	if len(mcpConfig) != 0 {
		t.Fatalf("mcp_config = %s, want nothing", mcpConfig)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "has no owner") {
		t.Fatalf("notices = %v", notices)
	}
}

// Once an OAuth server is configured under a name, the agent's other MCP
// config cannot supply that name: when the OAuth server cannot be mounted
// (needs reconnecting, or not connected), the same-named static entry is not
// mounted either. Falling back to it would run the task under the old shared
// credential right after the owner's grant was revoked.
func TestClaimWithholdsSameNamedEntriesWhenNotMounted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-withhold", `{"mcpServers":{`+
		`"own":{"url":"https://own.example"},`+
		`"dead":{"url":"https://dead.example","headers":{"Authorization":"Bearer static-shared-key-dead"}},`+
		`"pending":{"url":"https://pending.example","headers":{"Authorization":"Bearer static-shared-key-pending"}}}}`)
	connectForClaimTest(t, store, agentID, "pending", "", nil)
	dead := connectForClaimTest(t, store, agentID, "dead", "claim-token-revoked", nil)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE mcp_oauth_connection SET status = 'needs_reconnect', access_token_ciphertext = NULL,
			refresh_token_ciphertext = NULL, expires_at = NULL
		WHERE server_id = $1`, dead.ID); err != nil {
		t.Fatal(err)
	}

	agent := claimTaskAgentForTest(t, runtimeID)
	servers := decodeServers(t, agent.McpConfig)
	if len(servers) != 1 || servers["own"] == nil {
		t.Fatalf("servers = %v, want only own", serverNames(servers))
	}
	if strings.Contains(string(agent.McpConfig), "static-shared-key") {
		t.Fatalf("a withheld entry's credential reached the claim: %s", agent.McpConfig)
	}
	notices := strings.Join(agent.MCPOAuthNotices, "\n")
	if len(agent.MCPOAuthNotices) != 2 ||
		!strings.Contains(notices, `"pending" was not mounted: it is not connected yet`) ||
		!strings.Contains(notices, `"dead" was not mounted: its authorization expired or was revoked`) {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
}

// When the withheld entry was the agent's only MCP server, the claim still
// carries a managed, empty config, so the runtime does not fall back to an
// MCP config of its own.
func TestClaimWithholdingTheOnlyEntryLeavesAnEmptyManagedConfig(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-only",
		`{"mcpServers":{"gateway":{"url":"https://static.example","headers":{"Authorization":"Bearer static-shared-key"}}}}`)
	connectForClaimTest(t, store, agentID, "gateway", "", nil)

	agent := claimTaskAgentForTest(t, runtimeID)
	if len(agent.McpConfig) == 0 {
		t.Fatal("mcp_config is absent; the runtime would use its own MCP config")
	}
	if servers := decodeServers(t, agent.McpConfig); len(servers) != 0 {
		t.Fatalf("servers = %v, want none", serverNames(servers))
	}
	if len(agent.MCPOAuthNotices) != 1 {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
}

// Without the deployment key nothing can be mounted, and the names are still
// withheld from the agent's other config.
func TestClaimWithoutMCPOAuthKeyWithholdsAndNotes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-nokey",
		`{"mcpServers":{"own":{"url":"https://own.example"},"gateway":{"url":"https://static.example"}}}`)
	connectForClaimTest(t, store, agentID, "gateway", "claim-token-nokey", nil)
	t.Setenv(mcpoauth.SecretKeyEnv, "")

	agent := claimTaskAgentForTest(t, runtimeID)
	servers := decodeServers(t, agent.McpConfig)
	if len(servers) != 1 || servers["own"] == nil {
		t.Fatalf("servers = %v, want only own", serverNames(servers))
	}
	if len(agent.MCPOAuthNotices) != 1 || !strings.Contains(agent.MCPOAuthNotices[0], "not configured on this Multica server") {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
}

// failingDBExecutor fails every query, like a dropped connection.
type failingDBExecutor struct{}

var errFailingDB = errors.New("connection reset by peer")

func (failingDBExecutor) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errFailingDB
}

func (failingDBExecutor) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errFailingDB
}

func (failingDBExecutor) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingRow{}
}

type failingRow struct{}

func (failingRow) Scan(...any) error { return errFailingDB }

// When the agent's OAuth server names cannot be read, it is unknown which
// entries they own, so none is mounted: the claim carries an empty managed
// config (no fallback to a static entry, nor to the runtime's own config)
// and a notice. Pure unit test: the handler's database fails every query.
func TestClaimMCPOAuthNamesUnreadableMountsNothing(t *testing.T) {
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.MCPOAuthV1, featureflag.Rule{Default: true})
	h := &Handler{FeatureFlags: featureflag.NewService(provider), DB: failingDBExecutor{}}
	static := json.RawMessage(`{"mcpServers":{"gateway":{"url":"https://static.example","headers":{"Authorization":"Bearer static-shared-key"}},"own":{"url":"https://own.example"}}}`)

	mcpConfig, notices := h.applyMCPOAuthServers(context.Background(), &db.AgentTaskQueue{}, db.Agent{}, static)
	if !jsonEqual(t, mcpConfig, `{"mcpServers":{}}`) {
		t.Fatalf("mcp_config = %s, want an empty managed config", mcpConfig)
	}
	if len(notices) != 1 || notices[0] != mcpOAuthUnreadableNotice {
		t.Fatalf("notices = %v", notices)
	}
}

func TestWithoutMCPServers(t *testing.T) {
	task := &db.AgentTaskQueue{}
	names := []string{"gateway"}
	for name, tc := range map[string]struct{ in, want string }{
		"removes the name, keeps the rest": {
			`{"mcpServers":{"gateway":{"url":"https://a"},"own":{"url":"https://b"}},"other":1}`,
			`{"mcpServers":{"own":{"url":"https://b"}},"other":1}`,
		},
		"leaves an empty managed config":    {`{"mcpServers":{"gateway":{"url":"https://a"}}}`, `{"mcpServers":{}}`},
		"untouched when the name is absent": {`{"mcpServers":{"own":{"url":"https://b"}}}`, `{"mcpServers":{"own":{"url":"https://b"}}}`},
		"unmanaged stays unmanaged":         {``, ``},
		"unparsable is withheld":            {`{"mcpServers":`, `{"mcpServers":{}}`},
		"unparsable servers are withheld":   {`{"mcpServers":[1]}`, `{"mcpServers":{}}`},
	} {
		got := withoutMCPServers(json.RawMessage(tc.in), names, task)
		if !jsonEqual(t, got, tc.want) {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	if len(got) == 0 || want == "" {
		return len(got) == 0 && want == ""
	}
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("got %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestMCPOAuthNoticeWording(t *testing.T) {
	for err, want := range map[error]string{
		mcpoauth.ErrNotConnected:     "not connected yet",
		mcpoauth.ErrNeedsReconnect:   "Reconnect it",
		mcpoauth.ErrServerUnreadable: "Save the server again",
		mcpoauth.ErrUnavailable:      "The next task will try again",
		errors.New("anything else"):  "The next task will try again",
	} {
		got := mcpOAuthNotice("gw", err)
		if !strings.Contains(got, want) || !strings.Contains(got, `"gw"`) {
			t.Errorf("notice for %v = %q, want it to contain %q", err, got, want)
		}
	}
}
