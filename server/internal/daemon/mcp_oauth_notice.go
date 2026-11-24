// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
)

// reportMCPOAuthNotices writes one transcript line for each OAuth MCP server
// the server left out of this run (not connected, needs reconnecting, no
// token right now), so the person reading the run sees why a tool is missing
// instead of the agent silently doing without it.
//
// local are the daemon's own notices (a proxy that could not start, a token
// it could not fetch), written after the server's.
func (d *Daemon) reportMCPOAuthNotices(ctx context.Context, task Task, local []string, msgSeq *atomic.Int32, taskLog *slog.Logger) {
	if task.Agent != nil {
		for _, notice := range task.Agent.MCPOAuthNotices {
			d.reportTaskNotice(ctx, task.ID, notice, msgSeq, taskLog)
		}
	}
	for _, notice := range local {
		d.reportTaskNotice(ctx, task.ID, notice, msgSeq, taskLog)
	}
}

// mountMCPOAuthServers applies the claim's OAuth MCP decisions to the run's
// MCP config (applyMCPOAuthServers), fetching tokens from the server for
// this task.
func (d *Daemon) mountMCPOAuthServers(task Task, provider string, effective json.RawMessage, taskLog *slog.Logger) (json.RawMessage, *mcpOAuthProxySet, []string) {
	return applyMCPOAuthServers(mcpOAuthMountInput{
		Agent:     task.Agent,
		Provider:  provider,
		Effective: effective,
		RuntimeConfig: func(config json.RawMessage) (json.RawMessage, error) {
			return mergeRuntimeAndAgentMcpConfig(provider, config)
		},
		Fetch: func(ctx context.Context, serverID string) (mcpOAuthCredential, error) {
			return d.client.ResolveMCPOAuthCredential(ctx, task.ID, serverID)
		},
		Logger: taskLog,
	})
}
