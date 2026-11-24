// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// mcpOAuthClaimBudget bounds what the OAuth servers add to a claim. A
	// refresh that takes longer keeps running detached (TokenSource) and
	// its result is there for the next claim; this one mounts the server
	// without it.
	mcpOAuthClaimBudget = 5 * time.Second
	// mcpOAuthClaimMargin is how long an injected token should still have:
	// a token with less is refreshed first. TokenSource caps it at half the
	// token's lifetime, so with one-hour tokens a task starts with at least
	// 30 minutes. It applies only to a daemon without the proxy; with the
	// proxy a token expiring mid-task is replaced on the next call.
	mcpOAuthClaimMargin = 30 * time.Minute
)

// MCPOAuthProxyServer is an OAuth MCP server the daemon serves to the
// runtime through a local proxy, fetching the token for each call.
type MCPOAuthProxyServer struct {
	Name     string `json:"name"`
	ServerID string `json:"server_id"`
	URL      string `json:"url"`
}

// mcpOAuthClaim is what the OAuth MCP servers contribute to a claim.
type mcpOAuthClaim struct {
	// Config is the agent's mcp_config with every OAuth server name
	// withheld and, for a daemon without the proxy, the mountable servers
	// added with their tokens.
	Config  json.RawMessage
	Notices []string
	// Proxy lists the mountable servers for a daemon that proxies them; it
	// carries no token.
	Proxy []MCPOAuthProxyServer
	// Names are all the names OAuth servers are configured under, mounted
	// or not. The daemon withholds them from the runtime's own MCP config
	// too.
	Names []string
	// WithholdAll means the names could not be read: the daemon mounts no
	// MCP server at all for this run.
	WithholdAll bool
}

// applyMCPOAuthServers works out the agent's OAuth MCP servers for a claim,
// using the agent owner's grant.
//
// Every name an OAuth server is configured under belongs to that server from
// then on: any same-named entry the agent's other MCP config carries is
// withheld first, whatever happens next, and a proxying daemon withholds it
// from the runtime's own config as well. A server whose credential cannot be
// had this time (not connected, needs reconnecting, no owner, no token now,
// no deployment key) is then simply not mounted, and named in a notice for
// the run transcript. Falling back to the old entry instead would quietly run
// the task under whatever static credential that entry holds, after the owner
// disconnected or the grant was revoked. The task itself never fails over it.
//
// With proxy (the daemon advertised protocol.DaemonCapabilityMCPOAuthProxyV1)
// no token goes into the claim: the daemon fetches one per call, so a token
// expiring mid-task is replaced and none is written to the runtime's config
// files. Without it, the token is put in the entry's headers, refreshed first
// when less than mcpOAuthClaimMargin remains.
//
// With mcp_oauth_v1 off it returns mcpConfig untouched without touching the
// database: the claim behaves as upstream. Nothing here writes to
// agent_task_queue.
func (h *Handler) applyMCPOAuthServers(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, mcpConfig json.RawMessage, proxy bool) mcpOAuthClaim {
	if !featureflags.MCPOAuthV1Enabled(ctx, h.FeatureFlags) {
		return mcpOAuthClaim{Config: mcpConfig}
	}
	workspaceID, agentID := uuidToString(agent.WorkspaceID), uuidToString(agent.ID)
	names, err := mcpoauth.ListServerNamesForAgent(ctx, h.DB, workspaceID, agentID)
	if err != nil {
		// Without the names it is unknown which entries of mcpConfig an OAuth
		// server owns, and mounting any of them could put a withheld static
		// credential back into use. So this run gets no managed MCP servers
		// at all: an empty managed config, which also keeps the runtime from
		// falling back to its own. The task still runs.
		slog.Warn("daemon claim: list mcp oauth servers failed; mounting no MCP servers this run", "task_id", uuidToString(task.ID), "error", err)
		return mcpOAuthClaim{Config: json.RawMessage(`{"mcpServers":{}}`), Notices: []string{mcpOAuthUnreadableNotice}, WithholdAll: true}
	}
	if len(names) == 0 {
		return mcpOAuthClaim{Config: mcpConfig}
	}
	out := mcpOAuthClaim{Config: withoutMCPServers(mcpConfig, names, task), Names: names}

	notMounted := func(format string) []string {
		lines := make([]string, 0, len(names))
		for _, name := range names {
			lines = append(lines, fmt.Sprintf(format, name))
		}
		return lines
	}
	box, err := mcpOAuthBox()
	if err != nil {
		slog.Warn("daemon claim: mcp oauth key not configured; OAuth MCP servers not mounted", "task_id", uuidToString(task.ID))
		out.Notices = notMounted("MCP server %q was not mounted: OAuth MCP is not configured on this Multica server. Ask an administrator.")
		return out
	}
	store, err := mcpoauth.NewStore(mcpOAuthDatabase{h.DB, h.TxStarter}, box)
	if err != nil {
		out.Notices = notMounted("MCP server %q was not mounted: OAuth MCP is not configured on this Multica server. Ask an administrator.")
		return out
	}
	servers, err := store.ListServersForAgent(ctx, workspaceID, agentID)
	if err != nil {
		slog.Warn("daemon claim: list mcp oauth servers failed", "task_id", uuidToString(task.ID), "error", err)
		out.Notices = notMounted("MCP server %q was not mounted this time: its configuration could not be read. The next task will try again.")
		return out
	}

	budget, cancel := context.WithTimeout(ctx, mcpOAuthClaimBudget)
	defer cancel()
	source := mcpoauth.NewTokenSource(store, mcpoauth.Refresh, slog.Default())
	if !proxy {
		source.Margin = mcpOAuthClaimMargin
	}

	entries := make(map[string]any, len(servers))
	for _, server := range servers {
		if !agent.OwnerID.Valid {
			out.Notices = append(out.Notices, fmt.Sprintf("MCP server %q was not mounted: the agent has no owner whose connection it could use.", server.Name))
			continue
		}
		// With the proxy this still runs: it confirms the grant is usable
		// (refreshing it if due) so a dead connection gets its notice now
		// rather than as failing tool calls.
		credential, err := source.Credential(budget, server.ID, uuidToString(agent.OwnerID))
		if err != nil {
			out.Notices = append(out.Notices, mcpOAuthNotice(server.Name, err))
			slog.Info("daemon claim: mcp oauth server not mounted", "task_id", uuidToString(task.ID), "server_id", server.ID, "reason", mcpOAuthReason(err))
			continue
		}
		if proxy {
			out.Proxy = append(out.Proxy, MCPOAuthProxyServer{Name: server.Name, ServerID: server.ID, URL: server.URL})
			continue
		}
		entries[server.Name] = map[string]any{"type": "http", "url": server.URL, "headers": credential.Headers()}
	}
	if len(entries) == 0 {
		return out
	}
	overlay, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return out
	}
	merged, err := mergeMCPOverlay(out.Config, overlay)
	if err != nil {
		// out.Config no longer holds any of these names, so leaving the
		// servers out is still "not mounted", never a fallback.
		slog.Warn("daemon claim: merge mcp oauth servers failed; mounting none of them", "task_id", uuidToString(task.ID), "error", err)
		out.Notices = notMounted("MCP server %q was not mounted this time: the agent's MCP configuration could not be combined with it.")
		return out
	}
	out.Config = merged
	return out
}

// withoutMCPServers returns mcpConfig with the named entries removed from its
// mcpServers. When that empties mcpServers it stays as an empty object, so
// the runtime still receives a managed config rather than falling back to
// its own. A config that does not parse is replaced by an empty managed one:
// it cannot be checked for these names, and the runtime could not have used
// it as it was anyway.
func withoutMCPServers(mcpConfig json.RawMessage, names []string, task *db.AgentTaskQueue) json.RawMessage {
	if !hasManagedJSON(mcpConfig) {
		return mcpConfig
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(mcpConfig, &top); err != nil {
		slog.Warn("daemon claim: agent mcp_config does not parse; withholding it for its OAuth servers", "task_id", uuidToString(task.ID), "error", err)
		return json.RawMessage(`{"mcpServers":{}}`)
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := top["mcpServers"]; ok && hasManagedJSON(raw) {
		if err := json.Unmarshal(raw, &servers); err != nil {
			slog.Warn("daemon claim: agent mcpServers does not parse; withholding it for its OAuth servers", "task_id", uuidToString(task.ID), "error", err)
			return json.RawMessage(`{"mcpServers":{}}`)
		}
	}
	removed := false
	for _, name := range names {
		if _, ok := servers[name]; ok {
			delete(servers, name)
			removed = true
		}
	}
	if !removed {
		return mcpConfig
	}
	rawServers, err := json.Marshal(servers)
	if err != nil {
		return json.RawMessage(`{"mcpServers":{}}`)
	}
	top["mcpServers"] = rawServers
	out, err := json.Marshal(top)
	if err != nil {
		return json.RawMessage(`{"mcpServers":{}}`)
	}
	return out
}

// mcpOAuthUnreadableNotice is the transcript line when the agent's OAuth
// servers could not be read and no MCP server was mounted.
const mcpOAuthUnreadableNotice = "No MCP servers were mounted for this run: the agent's OAuth MCP configuration could not be read. The next task will try again."

// mcpOAuthNotice is the transcript line for a server left out of the task.
func mcpOAuthNotice(name string, err error) string {
	switch {
	case errors.Is(err, mcpoauth.ErrNotConnected):
		return fmt.Sprintf("MCP server %q was not mounted: it is not connected yet. The agent's owner can connect it in the agent's MCP settings.", name)
	case errors.Is(err, mcpoauth.ErrNeedsReconnect):
		return fmt.Sprintf("MCP server %q was not mounted: its authorization expired or was revoked. Reconnect it in the agent's MCP settings.", name)
	case errors.Is(err, mcpoauth.ErrServerUnreadable):
		return fmt.Sprintf("MCP server %q was not mounted: its saved headers can no longer be read. Save the server again in the agent's MCP settings.", name)
	default:
		return fmt.Sprintf("MCP server %q was not mounted this time: a current token could not be obtained. The next task will try again.", name)
	}
}

// mcpOAuthReason is a fixed code for logs; error text from the network is
// not logged.
func mcpOAuthReason(err error) string {
	switch {
	case errors.Is(err, mcpoauth.ErrNotConnected):
		return "not_connected"
	case errors.Is(err, mcpoauth.ErrNeedsReconnect):
		return "needs_reconnect"
	case errors.Is(err, mcpoauth.ErrServerUnreadable):
		return "server_unreadable"
	case errors.Is(err, mcpoauth.ErrUnavailable):
		return "unavailable"
	default:
		return "error"
	}
}
