// Added by the mouse9527/multica fork: name the run an agent acted from on
// issue events (M-193).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestIssueEventsNameTheActingRun: an issue created or updated by an agent
// carries the run it acted from on the event; a member's write carries none.
func TestIssueEventsNameTheActingRun(t *testing.T) {
	agentID := createHandlerTestAgent(t, "M-193 actor task agent", nil)
	issueID := dbfx.Issue(t, "M-193 actor task issue")
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issueID)

	got := make(chan events.Event, 16)
	// The bus is shared with other tests: keep the agent's events, which only
	// this test causes, and the member's on this issue.
	keep := func(e events.Event) {
		if e.ActorID == agentID || eventIssueID(e) == issueID {
			select {
			case got <- e:
			default:
			}
		}
	}
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, keep)
	testHandler.Bus.Subscribe(protocol.EventIssueCreated, keep)
	next := func(what string) events.Event {
		t.Helper()
		select {
		case e := <-got:
			return e
		default:
			t.Fatalf("%s: no event published", what)
			return events.Event{}
		}
	}

	update := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "in_progress"}), "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, asRun(update, agentID, taskID)).Want(http.StatusOK)
	if e := next("agent update"); e.ActorID != agentID || e.ActorTaskID != taskID {
		t.Fatalf("agent update event actor=%s task=%q, want %s and %s", e.ActorID, e.ActorTaskID, agentID, taskID)
	}

	batch := newRequest(http.MethodPatch, "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{issueID},
		"updates":   map[string]any{"status": "in_review"},
	})
	testutil.Call(t, testHandler.BatchUpdateIssues, asRun(batch, agentID, taskID)).Want(http.StatusOK)
	if e := next("agent batch update"); e.ActorTaskID != taskID {
		t.Fatalf("agent batch update event task=%q, want %s", e.ActorTaskID, taskID)
	}

	create := newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "M-193 created by an agent run",
	})
	resp := testutil.Call(t, testHandler.CreateIssue, asRun(create, agentID, taskID)).Want(http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	resp.JSON(&created)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, created.ID)
	if e := next("agent create"); e.Type != protocol.EventIssueCreated || e.ActorTaskID != taskID {
		t.Fatalf("agent create event type=%s task=%q, want issue:created and %s", e.Type, e.ActorTaskID, taskID)
	}

	member := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "done"}), "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, member).Want(http.StatusOK)
	if e := next("member update"); e.ActorID != testUserID || e.ActorTaskID != "" {
		t.Fatalf("member update event actor=%s task=%q, want %s and no task", e.ActorID, e.ActorTaskID, testUserID)
	}
}

func eventIssueID(e events.Event) string {
	payload, _ := e.Payload.(map[string]any)
	switch issue := payload["issue"].(type) {
	case IssueResponse:
		return issue.ID
	case *IssueResponse:
		return issue.ID
	}
	return ""
}
