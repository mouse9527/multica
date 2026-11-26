// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: correct what the claim margin promises (M-279).
// Modified for the mouse9527/multica fork: grants bound to the entry's pre-registered client (M-295).

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
	// mcpOAuthClaimBudget bounds what the OAuth entries add to a claim. A
	// refresh that takes longer keeps running detached (TokenSource) and
	// its result is there for the next claim; this one mounts the entry
	// without it.
	mcpOAuthClaimBudget = 5 * time.Second
	// mcpOAuthClaimMargin applies only to a daemon without the proxy, which
	// gets the token in the claim and keeps it for the whole task: a stored
	// token expiring within the margin is refreshed before it is injected.
	// TokenSource caps the margin at half the token's lifetime, so with
	// one-hour tokens the injected token has at least 30 minutes left —
	// unless no refresh can be made (a recent transient failure, no refresh
	// token), in which case the current token goes in with what it has left.
	//
	// With the proxy no token goes into the claim, and nothing here promises
	// a remaining lifetime: the claim and the daemon's per-call credential
	// fetch (mcp_oauth_daemon.go) both use TokenSource's DefaultMargin, so a
	// token the daemon gets may have only a few minutes left. The daemon
	// caches it until shortly before it expires and fetches again after that
	// or on a 401, so an expired token is not sent.
	mcpOAuthClaimMargin = 30 * time.Minute
)

// MCPOAuthProxyServer is an OAuth MCP entry the daemon serves to the runtime
// through a local proxy, fetching the token for each call. ServerID is the
// entry's proxy handle (mcpoauth.ProxyHandle: its ref bound to URL), which the
// daemon passes back to the credential endpoint, so the token it gets is only
// ever one for the URL it proxies to.
type MCPOAuthProxyServer struct {
	Name     string `json:"name"`
	ServerID string `json:"server_id"`
	URL      string `json:"url"`
}

// mcpOAuthClaim is what the OAuth entries contribute to a claim.
type mcpOAuthClaim struct {
	// Config is the MCP config with every OAuth entry taken out and, for a
	// daemon without the proxy, the mountable ones put back with their
	// tokens.
	Config  json.RawMessage
	Notices []string
	// Proxy lists the mountable entries for a daemon that proxies them; it
	// carries no token.
	Proxy []MCPOAuthProxyServer
	// Names are the names of all OAuth entries, mounted or not. The daemon
	// withholds them from the runtime's own MCP config too.
	Names []string
}

// applyMCPOAuthServers turns the OAuth entries of the MCP config a claim
// assembled — the agent's own entries and its bound workspace entries — into
// mountable servers, using the agent owner's connections: an agent entry's
// own connection, and for a workspace entry the owner's one connection that
// all of the owner's agents share.
//
// Every OAuth entry is first taken out of the config; it holds no credential
// of its own, and the runtime must never be handed one to call
// unauthenticated. An entry whose credential can be had is then put back
// (without the proxy) or listed for the proxy; one that cannot (not
// connected, needs reconnecting, its URL changed since it was connected, no
// owner, no token now, no deployment key, an invalid entry) is not mounted
// and is named in a notice for the run transcript. The task never fails over
// it. A switched-off entry is simply left out.
//
// With proxy (the daemon advertised protocol.DaemonCapabilityMCPOAuthProxyV1)
// no token goes into the claim: the daemon fetches one per call, so a token
// expiring mid-task is replaced and none is written to the runtime's config
// files. Without it, the token is put in the entry's headers, refreshed first
// when less than mcpOAuthClaimMargin remains.
//
// A config without OAuth entries is returned untouched without touching the
// database, and so is every config with mcp_oauth_v1 off: the claim behaves
// as upstream. Nothing here writes to agent_task_queue.
func (h *Handler) applyMCPOAuthServers(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, mcpConfig json.RawMessage, proxy bool) mcpOAuthClaim {
	if !featureflags.MCPOAuthV1Enabled(ctx, h.FeatureFlags) {
		return mcpOAuthClaim{Config: mcpConfig}
	}
	doc, servers, ok := mcpServersOf(mcpConfig)
	if !ok {
		return mcpOAuthClaim{Config: mcpConfig}
	}
	entries := mcpOAuthEntriesIn(servers)
	if len(entries) == 0 {
		return mcpOAuthClaim{Config: mcpConfig}
	}
	out := mcpOAuthClaim{}
	for _, entry := range entries {
		out.Names = append(out.Names, entry.Name)
		delete(servers, entry.Name)
	}
	resolveMCPOAuthSources(ctx, agent, &agentMcpBindings{h: h, agentID: agent.ID}, entries)

	var source *mcpoauth.TokenSource
	if box, err := mcpOAuthBox(); err == nil {
		if store, err := mcpoauth.NewStore(mcpOAuthDatabase{h.DB, h.TxStarter}, box); err == nil {
			source = mcpoauth.NewTokenSource(store, mcpoauth.Refresh, slog.Default())
			if !proxy {
				source.Margin = mcpOAuthClaimMargin
			}
		}
	}
	if source == nil {
		slog.Warn("daemon claim: mcp oauth key not configured; OAuth MCP servers not mounted", "task_id", uuidToString(task.ID))
	}
	budget, cancel := context.WithTimeout(ctx, mcpOAuthClaimBudget)
	defer cancel()
	workspaceID, ownerID := uuidToString(agent.WorkspaceID), uuidToString(agent.OwnerID)

	for _, entry := range entries {
		switch {
		case entry.Entry.Disabled && entry.Err == nil:
			continue
		case entry.Err != nil:
			out.Notices = append(out.Notices, fmt.Sprintf("MCP server %q was not mounted: its OAuth settings are not valid. Fix the entry in the MCP settings.", entry.Name))
			continue
		case entry.Ref == "":
			out.Notices = append(out.Notices, fmt.Sprintf("MCP server %q was not mounted this time: its configuration could not be read. The next task will try again.", entry.Name))
			continue
		case !agent.OwnerID.Valid:
			out.Notices = append(out.Notices, fmt.Sprintf("MCP server %q was not mounted: the agent has no owner whose connection it could use.", entry.Name))
			continue
		case source == nil:
			out.Notices = append(out.Notices, fmt.Sprintf("MCP server %q was not mounted: OAuth MCP is not configured on this Multica server. Ask an administrator.", entry.Name))
			continue
		}
		// With the proxy this still runs: it confirms the grant is usable
		// (refreshing it if due) so a dead connection gets its notice now
		// rather than as failing tool calls.
		target := mcpoauth.NewTarget(workspaceID, entry.Ref, entry.Entry)
		credential, err := source.Credential(budget, target, ownerID)
		if err != nil {
			out.Notices = append(out.Notices, mcpOAuthNotice(entry.Name, err))
			slog.Info("daemon claim: mcp oauth server not mounted", "task_id", uuidToString(task.ID), "server_ref", entry.Ref, "reason", mcpOAuthReason(err))
			continue
		}
		if proxy {
			out.Proxy = append(out.Proxy, MCPOAuthProxyServer{Name: entry.Name, ServerID: mcpoauth.ProxyHandle(entry.Ref, entry.Entry.URL), URL: entry.Entry.URL})
			continue
		}
		mounted, err := json.Marshal(map[string]any{"type": "http", "url": entry.Entry.URL, "headers": credential.Headers()})
		if err != nil {
			continue
		}
		servers[entry.Name] = mounted
	}
	out.Config = mcpConfigWithServers(doc, servers)
	return out
}

// mcpConfigWithServers writes servers back as the document's only container,
// "mcpServers" (the legacy "mcp" was folded into it by mcpServersOf). When no
// server is left it stays an empty object, so the runtime still receives a
// managed config rather than falling back to its own.
func mcpConfigWithServers(doc, servers map[string]json.RawMessage) json.RawMessage {
	out := make(map[string]json.RawMessage, len(doc)+1)
	for key, value := range doc {
		if key == "mcpServers" || key == "mcp" {
			continue
		}
		out[key] = value
	}
	rawServers, err := json.Marshal(servers)
	if err != nil {
		return json.RawMessage(`{"mcpServers":{}}`)
	}
	out["mcpServers"] = rawServers
	final, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{"mcpServers":{}}`)
	}
	return final
}

// mcpOAuthNotice is the transcript line for an entry left out of the task.
func mcpOAuthNotice(name string, err error) string {
	switch {
	case errors.Is(err, mcpoauth.ErrNotConnected):
		return fmt.Sprintf("MCP server %q was not mounted: it is not connected yet. The agent's owner can connect it in the MCP settings.", name)
	case errors.Is(err, mcpoauth.ErrNeedsReconnect):
		return fmt.Sprintf("MCP server %q was not mounted: its authorization expired, was revoked, or its URL changed. Reconnect it in the MCP settings.", name)
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
	case errors.Is(err, mcpoauth.ErrUnavailable):
		return "unavailable"
	default:
		return "error"
	}
}
