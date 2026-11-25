// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

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
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// oauthEntry is an MCP entry that uses OAuth, for an mcp_config or a
// workspace library entry.
func oauthEntry(url string) string {
	return `{"type":"http","url":"` + url + `","auth":{"type":"oauth"}}`
}

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

func claimTaskAgentForTest(t *testing.T, runtimeID string, capabilities ...string) *TaskAgentData {
	t.Helper()
	w := httptest.NewRecorder()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	if len(capabilities) > 0 {
		req.Header.Set("X-Client-Capabilities", strings.Join(capabilities, ","))
	}
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

// connectForTest saves userID's grant for the entry ref, made for url.
func connectForTest(t *testing.T, store *mcpoauth.Store, ref, url, userID, accessToken string) {
	t.Helper()
	if err := store.SaveGrant(context.Background(), mcpoauth.Grant{
		WorkspaceID: testWorkspaceID, ServerRef: ref, ServerURL: url, UserID: userID,
		Resource: url, TokenEndpoint: "https://mcp.example.com/token",
		Client: mcpoauth.Client{ID: "client"}, AccessToken: mcpoauth.NewSecret(accessToken),
		RefreshToken: mcpoauth.NewSecret("refresh-" + accessToken), ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
}

func markNeedsReconnect(t *testing.T, ref string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		UPDATE mcp_oauth_connection SET status = 'needs_reconnect', access_token_ciphertext = NULL,
			refresh_token_ciphertext = NULL, expires_at = NULL
		WHERE server_ref = $1`, ref); err != nil {
		t.Fatal(err)
	}
}

// workspaceOAuthEntryForTest is a workspace library entry using OAuth, with
// its connections cleaned up after the test.
func workspaceOAuthEntryForTest(t *testing.T, name, url string) (serverID, ref string) {
	t.Helper()
	serverID = createWorkspaceMcpServerForTest(t, name, oauthEntry(url))
	ref = mcpoauth.WorkspaceRef(serverID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM mcp_oauth_connection WHERE server_ref = $1`, ref)
		testPool.Exec(context.Background(), `DELETE FROM mcp_oauth_state WHERE server_ref = $1`, ref)
	})
	return serverID, ref
}

func bindForTest(t *testing.T, agentID, serverID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `INSERT INTO agent_mcp_server (agent_id, server_id) VALUES ($1, $2)`, agentID, serverID); err != nil {
		t.Fatal(err)
	}
}

// With the flag off the claim is upstream's: the entries go out exactly as
// saved, OAuth ones included, and nothing is read from the OAuth tables.
func TestClaimWithoutMCPOAuthFlagIsUpstream(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, false)
	config := `{"mcpServers":{"own":{"url":"https://own.example"},"gateway":` + oauthEntry("https://mcp.example.com/gateway") + `}}`
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-off", config)
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), "https://mcp.example.com/gateway", testUserID, "claim-token-off")

	agent := claimTaskAgentForTest(t, runtimeID)
	if !mcpConfigJSONEqual(t, agent.McpConfig, config) || len(agent.MCPOAuthNotices) != 0 || len(agent.MCPOAuthNames) != 0 {
		t.Fatalf("mcp_config = %s, notices = %v; want the saved config as is", agent.McpConfig, agent.MCPOAuthNotices)
	}
}

// A connected entry is mounted with the owner's token and nothing else of
// OAuth's; entries that cannot be mounted are left out and named in a
// notice; a switched-off entry is left out quietly; the token is nowhere in
// the task row.
func TestClaimMountsConnectedMCPOAuthEntries(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, taskID := mcpOAuthClaimFixture(t, "mcp-oauth-claim-on", `{"mcpServers":{`+
		`"own":{"url":"https://own.example"},`+
		`"gateway":`+oauthEntry("https://mcp.example.com/gateway")+`,`+
		`"pending":`+oauthEntry("https://mcp.example.com/pending")+`,`+
		`"dead":`+oauthEntry("https://mcp.example.com/dead")+`,`+
		`"off":{"url":"https://mcp.example.com/off","enabled":false,"auth":{"type":"oauth"}}}}`)
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), "https://mcp.example.com/gateway", testUserID, "claim-token-live")
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "dead"), "https://mcp.example.com/dead", testUserID, "claim-token-dead")
	markNeedsReconnect(t, mcpoauth.AgentRef(agentID, "dead"))

	agent := claimTaskAgentForTest(t, runtimeID)
	servers := decodeServers(t, agent.McpConfig)
	if len(servers) != 2 || servers["own"] == nil || servers["gateway"] == nil {
		t.Fatalf("servers = %v, want own and gateway", serverNames(servers))
	}
	entry, _ := servers["gateway"].(map[string]any)
	headers, _ := entry["headers"].(map[string]any)
	if entry["type"] != "http" || entry["url"] != "https://mcp.example.com/gateway" || entry["auth"] != nil ||
		headers["Authorization"] != "Bearer claim-token-live" || len(headers) != 1 {
		t.Fatalf("gateway entry = %v", entry)
	}

	notices := strings.Join(agent.MCPOAuthNotices, "\n")
	if len(agent.MCPOAuthNotices) != 2 ||
		!strings.Contains(notices, `"pending" was not mounted: it is not connected yet`) ||
		!strings.Contains(notices, `"dead" was not mounted: its authorization expired`) {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
	if strings.Contains(notices, "claim-token") {
		t.Fatal("a notice carries a token")
	}
	if strings.Join(agent.MCPOAuthNames, ",") != "dead,gateway,off,pending" {
		t.Fatalf("names = %v", agent.MCPOAuthNames)
	}

	var row string
	if err := testPool.QueryRow(context.Background(), `SELECT t::text FROM agent_task_queue t WHERE id = $1`, taskID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, "claim-token-live") {
		t.Fatal("the token was written to agent_task_queue")
	}
}

// A workspace entry is connected once by the owner and mounted for every
// agent of theirs it is bound to, with that one connection. An agent's own
// entry of the same name wins over it, as for any workspace entry.
func TestClaimSharesTheOwnersWorkspaceConnection(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	serverID, ref := workspaceOAuthEntryForTest(t, "shared-gateway", "https://mcp.example.com/shared")
	firstRuntime, firstAgent, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-shared-1", "")
	secondRuntime, secondAgent, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-shared-2", "")
	thirdRuntime, thirdAgent, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-shared-3", `{"mcpServers":{"shared-gateway":{"url":"https://own.example/plain"}}}`)
	for _, agentID := range []string{firstAgent, secondAgent, thirdAgent} {
		bindForTest(t, agentID, serverID)
	}
	connectForTest(t, store, ref, "https://mcp.example.com/shared", testUserID, "claim-token-shared")

	for _, runtimeID := range []string{firstRuntime, secondRuntime} {
		agent := claimTaskAgentForTest(t, runtimeID)
		entry, _ := decodeServers(t, agent.McpConfig)["shared-gateway"].(map[string]any)
		headers, _ := entry["headers"].(map[string]any)
		if headers["Authorization"] != "Bearer claim-token-shared" || len(agent.MCPOAuthNotices) != 0 {
			t.Fatalf("runtime %s: entry = %v, notices = %v", runtimeID, entry, agent.MCPOAuthNotices)
		}
	}
	agent := claimTaskAgentForTest(t, thirdRuntime)
	entry, _ := decodeServers(t, agent.McpConfig)["shared-gateway"].(map[string]any)
	if entry["url"] != "https://own.example/plain" || entry["headers"] != nil || len(agent.MCPOAuthNames) != 0 {
		t.Fatalf("the agent's own entry did not win: %v, names = %v", entry, agent.MCPOAuthNames)
	}
}

// A grant is bound to the URL it was made for: once the entry points
// elsewhere, the token is not sent there and the connection asks for a
// reconnect.
func TestClaimDoesNotSendATokenToAChangedURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-moved",
		`{"mcpServers":{"gateway":`+oauthEntry("https://attacker.example.com/mcp")+`}}`)
	ref := mcpoauth.AgentRef(agentID, "gateway")
	connectForTest(t, store, ref, "https://mcp.example.com/gateway", testUserID, "claim-token-moved")

	agent := claimTaskAgentForTest(t, runtimeID)
	if strings.Contains(string(agent.McpConfig), "claim-token-moved") || len(decodeServers(t, agent.McpConfig)) != 0 {
		t.Fatalf("mcp_config = %s", agent.McpConfig)
	}
	if len(agent.MCPOAuthNotices) != 1 || !strings.Contains(agent.MCPOAuthNotices[0], "its URL changed") {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
	status, err := store.GetConnectionStatus(context.Background(), testWorkspaceID, ref, testUserID)
	if err != nil || status.Status != mcpoauth.StatusNeedsReconnect || status.LastError != mcpoauth.ReasonURLChanged {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
}

// An entry that asks for OAuth but is not a valid OAuth entry (saved while
// the flag was off) is never mounted as a plain server.
func TestClaimWithholdsInvalidOAuthEntries(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	runtimeID, _, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-invalid", `{"mcpServers":{`+
		`"plain-http":{"url":"http://mcp.example.com/mcp","auth":{"type":"oauth"}},`+
		`"keyed":{"url":"https://mcp.example.com/mcp","headers":{"x-api-key":"static-shared-key"},"auth":{"type":"OAuth"}}}}`)

	agent := claimTaskAgentForTest(t, runtimeID)
	if len(decodeServers(t, agent.McpConfig)) != 0 || strings.Contains(string(agent.McpConfig), "static-shared-key") {
		t.Fatalf("mcp_config = %s, want no server", agent.McpConfig)
	}
	if len(agent.MCPOAuthNotices) != 2 || !strings.Contains(agent.MCPOAuthNotices[0], "OAuth settings are not valid") {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
}

// Entries in the legacy "mcp" container are found too, and the config the
// runtime gets is folded onto "mcpServers".
func TestClaimFindsOAuthEntriesInTheLegacyContainer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-legacy",
		`{"mcp":{"legacy":{"url":"https://legacy.example"},"gateway":`+oauthEntry("https://mcp.example.com/gateway")+`}}`)
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), "https://mcp.example.com/gateway", testUserID, "claim-token-legacy")

	agent := claimTaskAgentForTest(t, runtimeID)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(agent.McpConfig, &doc); err != nil {
		t.Fatal(err)
	}
	servers := decodeServers(t, agent.McpConfig)
	if _, legacy := doc["mcp"]; legacy || len(servers) != 2 || servers["legacy"] == nil || servers["gateway"] == nil {
		t.Fatalf("mcp_config = %s", agent.McpConfig)
	}
}

// An agent without an owner has no connection to use: its entries are left
// out with a notice. (A private runtime refuses an ownerless agent before
// this point, so the hook is called directly with the agent row.)
func TestClaimMCPOAuthWithoutOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withMCPOAuth(t, true)
	config := `{"mcpServers":{"gateway":` + oauthEntry("https://mcp.example.com/gateway") + `}}`
	_, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-ownerless", config)
	agent, err := testHandler.Queries.GetAgent(context.Background(), util.MustParseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	agent.OwnerID = pgtype.UUID{}
	result := testHandler.applyMCPOAuthServers(context.Background(), &db.AgentTaskQueue{}, agent, json.RawMessage(config), false)
	if !mcpConfigJSONEqual(t, result.Config, `{"mcpServers":{}}`) {
		t.Fatalf("mcp_config = %s, want an empty managed config", result.Config)
	}
	if len(result.Notices) != 1 || !strings.Contains(result.Notices[0], "has no owner") {
		t.Fatalf("notices = %v", result.Notices)
	}
}

// Without the deployment key nothing can be mounted; the OAuth entries are
// still taken out.
func TestClaimWithoutMCPOAuthKeyWithholdsAndNotes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-nokey",
		`{"mcpServers":{"own":{"url":"https://own.example"},"gateway":`+oauthEntry("https://mcp.example.com/gateway")+`}}`)
	connectForTest(t, store, mcpoauth.AgentRef(agentID, "gateway"), "https://mcp.example.com/gateway", testUserID, "claim-token-nokey")
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

// Pure unit test, the database failing every query: a config without OAuth
// entries is handed back untouched, reading nothing; an OAuth entry whose
// credential cannot be read is taken out, never passed on without a token.
func TestClaimMCPOAuthWithoutADatabase(t *testing.T) {
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.MCPOAuthV1, featureflag.Rule{Default: true})
	h := &Handler{FeatureFlags: featureflag.NewService(provider), DB: failingDBExecutor{}}
	t.Setenv(mcpoauth.SecretKeyEnv, "")

	plain := json.RawMessage(`{"mcpServers":{"own":{"url":"https://own.example","headers":{"Authorization":"Bearer static"}}}}`)
	if result := h.applyMCPOAuthServers(context.Background(), &db.AgentTaskQueue{}, db.Agent{}, plain, true); string(result.Config) != string(plain) || len(result.Notices) != 0 {
		t.Fatalf("plain config = %s, notices = %v", result.Config, result.Notices)
	}

	config := `{"mcpServers":{"own":{"url":"https://own.example"},"gateway":` + oauthEntry("https://mcp.example.com/gateway") + `}}`
	agent := db.Agent{McpConfig: []byte(config), OwnerID: util.MustParseUUID(testUserIDForUnit)}
	result := h.applyMCPOAuthServers(context.Background(), &db.AgentTaskQueue{}, agent, json.RawMessage(config), true)
	if servers := decodeServers(t, result.Config); len(servers) != 1 || servers["own"] == nil {
		t.Fatalf("servers = %v, want only own", serverNames(servers))
	}
	if len(result.Proxy) != 0 || len(result.Notices) != 1 || strings.Join(result.Names, ",") != "gateway" {
		t.Fatalf("result = %+v", result)
	}
}

// testUserIDForUnit is any valid owner id for the pure unit test above.
const testUserIDForUnit = "00000000-0000-0000-0000-0000000000aa"

func mcpConfigJSONEqual(t *testing.T, got json.RawMessage, want string) bool {
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

// A daemon that proxies OAuth MCP gets no token at all in the claim: the
// mountable entries come as a proxy list (name, handle binding the ref to the
// URL, URL), every OAuth name
// is listed for the daemon to withhold from the runtime's own config, and
// entries that cannot be mounted still get their notices.
func TestClaimForAProxyingDaemonCarriesNoToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	store := withMCPOAuth(t, true)
	runtimeID, agentID, _ := mcpOAuthClaimFixture(t, "mcp-oauth-claim-proxy", `{"mcpServers":{`+
		`"own":{"url":"https://own.example"},`+
		`"gateway":`+oauthEntry("https://mcp.example.com/gateway")+`,`+
		`"pending":`+oauthEntry("https://mcp.example.com/pending")+`}}`)
	ref := mcpoauth.AgentRef(agentID, "gateway")
	connectForTest(t, store, ref, "https://mcp.example.com/gateway", testUserID, "claim-token-proxy")

	agent := claimTaskAgentForTest(t, runtimeID, protocol.DaemonCapabilityMCPOAuthProxyV1)
	if strings.Contains(string(agent.McpConfig), "claim-token-proxy") {
		t.Fatalf("mcp_config carries a token: %s", agent.McpConfig)
	}
	if servers := decodeServers(t, agent.McpConfig); len(servers) != 1 || servers["own"] == nil {
		t.Fatalf("servers = %v, want only own", serverNames(servers))
	}
	if len(agent.MCPOAuthProxy) != 1 || agent.MCPOAuthProxy[0] != (MCPOAuthProxyServer{Name: "gateway", ServerID: mcpoauth.ProxyHandle(ref, "https://mcp.example.com/gateway"), URL: "https://mcp.example.com/gateway"}) {
		t.Fatalf("proxy = %+v", agent.MCPOAuthProxy)
	}
	if strings.Join(agent.MCPOAuthNames, ",") != "gateway,pending" {
		t.Fatalf("names = %v", agent.MCPOAuthNames)
	}
	if len(agent.MCPOAuthNotices) != 1 || !strings.Contains(agent.MCPOAuthNotices[0], `"pending"`) {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
}

func TestMCPOAuthNoticeWording(t *testing.T) {
	for err, want := range map[error]string{
		mcpoauth.ErrNotConnected:    "not connected yet",
		mcpoauth.ErrNeedsReconnect:  "Reconnect it",
		mcpoauth.ErrUnavailable:     "The next task will try again",
		errors.New("anything else"): "The next task will try again",
	} {
		got := mcpOAuthNotice("gw", err)
		if !strings.Contains(got, want) || !strings.Contains(got, `"gw"`) {
			t.Errorf("notice for %v = %q, want it to contain %q", err, got, want)
		}
	}
}
