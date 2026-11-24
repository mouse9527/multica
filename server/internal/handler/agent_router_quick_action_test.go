// Added by the mouse9527/multica fork: a quick action an agent runs records
// the run on its comment (M-233).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestQuickActionCommentRecordsTheAgentsRun: the comment a quick action posts
// records the run of the agent that ran it, as CreateComment does, so a Super
// Agent running one from its chat does not wake itself. A member's carries
// none.
func TestQuickActionCommentRecordsTheAgentsRun(t *testing.T) {
	var action QuickActionResponse
	testutil.Call(t, testHandler.CreateQuickAction, newRequest(http.MethodPost, "/api/quick-actions", map[string]any{
		"name": "M-233 check", "prompt": "Please check the latest changes.", "visibility": "public",
	})).Want(http.StatusCreated).JSON(&action)
	dbfx.Cleanup(t, `DELETE FROM quick_action WHERE id = $1`, action.ID)

	agentID := createHandlerTestAgent(t, "M-233 quick action agent", nil)
	issueID := dbfx.Issue(t, "M-233 quick action issue")
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issueID)
	run := func() *http.Request {
		return testutil.WithURLParams(newRequest(http.MethodPost, "/api/issues/"+issueID+"/quick-actions/"+action.ID+"/run", nil),
			"id", issueID, "quickActionId", action.ID)
	}

	sourceTask := func(commentID string) string {
		t.Helper()
		var task string
		dbfx.QueryRow(t, `SELECT COALESCE(source_task_id::text, '') FROM comment WHERE id = $1`, commentID).Scan(&task)
		return task
	}
	var byAgent CommentResponse
	testutil.Call(t, testHandler.RunQuickAction, asRun(run(), agentID, taskID)).Want(http.StatusCreated).JSON(&byAgent)
	if byAgent.AuthorType != "agent" || sourceTask(byAgent.ID) != taskID {
		t.Fatalf("agent's quick action comment: author %s, source task %q; want agent and %s", byAgent.AuthorType, sourceTask(byAgent.ID), taskID)
	}

	var byMember CommentResponse
	testutil.Call(t, testHandler.RunQuickAction, run()).Want(http.StatusCreated).JSON(&byMember)
	if byMember.AuthorType != "member" || sourceTask(byMember.ID) != "" {
		t.Fatalf("member's quick action comment: author %s, source task %q; want member and none", byMember.AuthorType, sourceTask(byMember.ID))
	}
}
