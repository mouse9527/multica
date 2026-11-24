// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
)

// Each notice from the claim becomes its own transcript row, in order and on
// the run's sequence, after any earlier daemon notice.
func TestReportMCPOAuthNotices(t *testing.T) {
	t.Parallel()
	d, rec := newTranscriptRecorder(t)
	var seq atomic.Int32
	seq.Store(1) // an earlier notice, such as the thinking-level warning
	task := Task{ID: "task-mcp", Agent: &AgentData{MCPOAuthNotices: []string{
		`MCP server "gateway" was not mounted: its authorization expired or was revoked.`,
		`MCP server "other" was not mounted: it is not connected yet.`,
	}}}
	d.reportMCPOAuthNotices(context.Background(), task, &seq, quietTaskLog())
	rows := rec.snapshot()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	for i, notice := range task.Agent.MCPOAuthNotices {
		if rows[i].Type != "text" || rows[i].Content != notice+"\n\n" || rows[i].Seq != i+2 {
			t.Fatalf("row %d = %+v", i, rows[i])
		}
	}
}

func TestReportMCPOAuthNoticesWithoutNotices(t *testing.T) {
	t.Parallel()
	d, rec := newTranscriptRecorder(t)
	var seq atomic.Int32
	d.reportMCPOAuthNotices(context.Background(), Task{ID: "task-none", Agent: &AgentData{}}, &seq, quietTaskLog())
	d.reportMCPOAuthNotices(context.Background(), Task{ID: "task-no-agent"}, &seq, quietTaskLog())
	if rows := rec.snapshot(); len(rows) != 0 || seq.Load() != 0 {
		t.Fatalf("rows = %+v, seq = %d; want nothing", rows, seq.Load())
	}
}

// The claim's field reaches AgentData under the name the server sends.
func TestAgentDataDecodesMCPOAuthNotices(t *testing.T) {
	t.Parallel()
	var agent AgentData
	if err := json.Unmarshal([]byte(`{"id":"a","mcp_oauth_notices":["n1"]}`), &agent); err != nil {
		t.Fatal(err)
	}
	if len(agent.MCPOAuthNotices) != 1 || agent.MCPOAuthNotices[0] != "n1" {
		t.Fatalf("notices = %v", agent.MCPOAuthNotices)
	}
}
