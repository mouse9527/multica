// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// reportMCPOAuthNotices writes one transcript line for each OAuth MCP server
// the server left out of this run (not connected, needs reconnecting, no
// token right now), so the person reading the run sees why a tool is missing
// instead of the agent silently doing without it.
func (d *Daemon) reportMCPOAuthNotices(ctx context.Context, task Task, msgSeq *atomic.Int32, taskLog *slog.Logger) {
	if task.Agent == nil {
		return
	}
	for _, notice := range task.Agent.MCPOAuthNotices {
		d.reportTaskNotice(ctx, task.ID, notice, msgSeq, taskLog)
	}
}
