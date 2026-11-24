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
	// 30 minutes. A token expiring mid-task is not renewed in this phase;
	// see the PR that adds the daemon-side proxy.
	mcpOAuthClaimMargin = 30 * time.Minute
)

// applyMCPOAuthServers adds the agent's connected OAuth MCP servers to the
// claim's mcp_config as `{type: "http", url, headers}` entries, using the
// agent owner's grant. A server it cannot mount is left out and named in a
// notice for the task transcript; the task itself never fails over it.
//
// With mcp_oauth_v1 off it returns mcpConfig untouched without touching the
// database: the claim behaves as upstream. The token travels only in this
// claim response; nothing here writes it to agent_task_queue.
func (h *Handler) applyMCPOAuthServers(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, mcpConfig json.RawMessage) (json.RawMessage, []string) {
	if !featureflags.MCPOAuthV1Enabled(ctx, h.FeatureFlags) {
		return mcpConfig, nil
	}
	box, err := mcpOAuthBox()
	if err != nil {
		slog.Warn("daemon claim: mcp oauth key not configured; OAuth MCP servers skipped", "task_id", uuidToString(task.ID))
		return mcpConfig, nil
	}
	store, err := mcpoauth.NewStore(mcpOAuthDatabase{h.DB, h.TxStarter}, box)
	if err != nil {
		return mcpConfig, nil
	}
	servers, err := store.ListServersForAgent(ctx, uuidToString(agent.WorkspaceID), uuidToString(agent.ID))
	if err != nil {
		slog.Warn("daemon claim: list mcp oauth servers failed", "task_id", uuidToString(task.ID), "error", err)
		return mcpConfig, nil
	}
	if len(servers) == 0 {
		return mcpConfig, nil
	}

	budget, cancel := context.WithTimeout(ctx, mcpOAuthClaimBudget)
	defer cancel()
	source := mcpoauth.NewTokenSource(store, mcpoauth.Refresh, slog.Default())
	source.Margin = mcpOAuthClaimMargin

	entries := make(map[string]any, len(servers))
	var notices []string
	for _, server := range servers {
		if !agent.OwnerID.Valid {
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted: the agent has no owner whose connection it could use.", server.Name))
			continue
		}
		credential, err := source.Credential(budget, server.ID, uuidToString(agent.OwnerID))
		if err != nil {
			notices = append(notices, mcpOAuthNotice(server.Name, err))
			slog.Info("daemon claim: mcp oauth server not mounted", "task_id", uuidToString(task.ID), "server_id", server.ID, "reason", mcpOAuthReason(err))
			continue
		}
		entries[server.Name] = map[string]any{"type": "http", "url": server.URL, "headers": credential.Headers()}
	}
	if len(entries) == 0 {
		return mcpConfig, notices
	}
	overlay, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return mcpConfig, notices
	}
	// The connected server replaces a same-named entry from the agent's own
	// config: it is the one the owner set up and connected for this name.
	merged, err := mergeMCPOverlay(mcpConfig, overlay)
	if err != nil {
		slog.Warn("daemon claim: merge mcp oauth servers failed; using mcp_config without them", "task_id", uuidToString(task.ID), "error", err)
		return mcpConfig, notices
	}
	return merged, notices
}

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
