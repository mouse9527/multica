// Added by the mouse9527/multica fork: one run per operation (M-280).

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// forkDedupAgent returns the workspace agent with a runtime that the other
// task tests use; every fixture below lives on its own issue.
func forkDedupAgent(t *testing.T) (agentID, runtimeID string) {
	t.Helper()
	dbfx.QueryRow(t,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL AND archived_at IS NULL ORDER BY created_at LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID)
	return agentID, runtimeID
}

// forkDedupIssue is an issue assigned to agentID whose tasks, including the
// ones the code under test enqueues, are removed after the test.
func forkDedupIssue(t *testing.T, title, status, agentID string) string {
	t.Helper()
	issueID := dbfx.Issue(t, title, testutil.Cols{
		"status":        status,
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM activity_log WHERE issue_id = $1`, issueID)
	return issueID
}

// forkDedupRun is a running assignment run of agentID bound to step, created
// and claimed at the given SQL times.
func forkDedupRun(t *testing.T, agentID, runtimeID, issueID, step, createdAt, dispatchedAt string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"issue_id":              issueID,
		"status":                "running",
		"trigger_evidence_kind": "issue_assignment",
		"workflow_step":         step,
		"created_at":            testutil.Raw(createdAt),
		"dispatched_at":         testutil.Raw(dispatchedAt),
		"started_at":            testutil.Raw(dispatchedAt),
	})
}

// forkDedupCommentRun is the queued run a top-level comment created at
// createdAt (a SQL expression) earned for agentID.
func forkDedupCommentRun(t *testing.T, agentID, runtimeID, issueID, createdAt string) string {
	t.Helper()
	commentID := dbfx.Comment(t, issueID, "please take it from here", testutil.Cols{
		"created_at": testutil.Raw(createdAt),
	})
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":              runtimeID,
		"issue_id":                issueID,
		"status":                  "queued",
		"trigger_comment_id":      commentID,
		"trigger_evidence_kind":   "comment",
		"trigger_evidence_ref_id": commentID,
		"created_at":              testutil.Raw(createdAt),
	})
}

func forkDedupTaskState(t *testing.T, taskID string) (status, failureReason string) {
	t.Helper()
	dbfx.QueryRow(t,
		`SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id = $1`,
		taskID).Scan(&status, &failureReason)
	return status, failureReason
}

func forkDedupComplete(t *testing.T, taskID string) {
	t.Helper()
	if w := completeTaskViaHandler(t, taskID, "done"); w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// forkDedupPostComment posts a member comment on issueID through the comment
// trigger path and returns its id.
func forkDedupPostComment(t *testing.T, issueID string) string {
	t.Helper()
	ctx := context.Background()
	commentID := dbfx.Comment(t, issueID, "please take it from here")
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	comment, err := testHandler.Queries.GetComment(ctx, parseUUID(commentID))
	if err != nil {
		t.Fatalf("load comment: %v", err)
	}
	testHandler.triggerTasksForComment(ctx, issue, comment, nil, "member", testUserID, testUserID, nil, commentSteerRequest{})
	return commentID
}

// forkDedupHandoff dispatches a workflow handoff run for the issue's agent
// assignee, as UpdateIssue does after moving it into a handing-off step.
func forkDedupHandoff(t *testing.T, issueID string) {
	t.Helper()
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	testHandler.dispatchIssueRun(ctx, issue, service.IssueRunTrigger{
		IssueID:         issue.ID,
		AgentID:         issue.AssigneeID,
		AssigneeType:    "agent",
		WorkflowHandoff: true,
	}, "member", testUserID, "")
}

// Scenario 2: the issue moves into the agent's step (its run is claimed at
// once) and a comment for the agent lands a second later. The comment's run
// supersedes the just-claimed one, so one run remains and its claim comes
// after both inputs.
func TestForkRunDedup_CommentRightAfterClaimSupersedesRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: comment after claim", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '2 seconds'", "now() - interval '1 second'")
	commentID := forkDedupPostComment(t, issueID)

	if status, reason := forkDedupTaskState(t, first); status != "cancelled" || reason != service.ForkSupersedeFailureReason {
		t.Fatalf("just-claimed run: got status %q reason %q, want cancelled %q", status, reason, service.ForkSupersedeFailureReason)
	}
	if n := dbfx.Count(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running')`,
		issueID, agentID); n != 1 {
		t.Fatalf("expected exactly one live run, got %d", n)
	}
	if n := dbfx.Count(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND trigger_comment_id = $3`,
		issueID, agentID, commentID); n != 1 {
		t.Fatalf("expected the comment's run to be queued, got %d", n)
	}
}

// A run claimed before the grace window may already be reading: the comment's
// run follows it, as upstream.
func TestForkRunDedup_LaterCommentLeavesRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: later comment", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '6 seconds'", "now() - interval '5 seconds'")
	forkDedupPostComment(t, issueID)

	if status, _ := forkDedupTaskState(t, first); status != "running" {
		t.Fatalf("run claimed 5s before the comment: got status %q, want running", status)
	}
	if n := queuedTaskCountForAgentIssue(t, issueID, agentID); n != 1 {
		t.Fatalf("expected the comment's run to be queued, got %d", n)
	}
}

// A comment supersedes only its own agent's run on its own issue.
func TestForkRunDedup_CommentLeavesOtherAgentsAndIssues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	otherRuntime := dbfx.Runtime(t, "run dedup other runtime")
	otherAgent := dbfx.Agent(t, "run dedup other agent", otherRuntime)
	issueID := forkDedupIssue(t, "run dedup: comment isolation", "in_review", agentID)
	otherIssueID := forkDedupIssue(t, "run dedup: comment isolation, other issue", "in_review", agentID)

	otherAgentRun := forkDedupRun(t, otherAgent, otherRuntime, issueID, "in_review", "now() - interval '2 seconds'", "now() - interval '1 second'")
	otherIssueRun := forkDedupRun(t, agentID, runtimeID, otherIssueID, "in_review", "now() - interval '2 seconds'", "now() - interval '1 second'")
	forkDedupPostComment(t, issueID)

	for name, id := range map[string]string{"other agent": otherAgentRun, "other issue": otherIssueRun} {
		if status, _ := forkDedupTaskState(t, id); status != "running" {
			t.Errorf("%s: got status %q, want running", name, status)
		}
	}
}

// With the flag off a comment leaves the just-claimed run alone, as upstream.
func TestForkRunDedup_FlagOffCommentKeepsRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, false)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: flag off, comment", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '2 seconds'", "now() - interval '1 second'")
	forkDedupPostComment(t, issueID)

	if status, _ := forkDedupTaskState(t, first); status != "running" {
		t.Fatalf("flag off: got status %q, want running", status)
	}
}

// A comment that was already on the issue when the run was claimed reached
// it through the claim: its queued run is folded when the run completes.
func TestForkRunDedup_CommentBeforeClaimFoldsIntoRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: comment before claim", "in_review", agentID)

	// Queued at -60s, claimed at -50s; the comment came at -55s.
	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '60 seconds'", "now() - interval '50 seconds'")
	second := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '55 seconds'")

	forkDedupComplete(t, first)

	if status, reason := forkDedupTaskState(t, second); status != "cancelled" || reason != service.ForkFoldFailureReason {
		t.Fatalf("comment run: got status %q reason %q, want cancelled %q", status, reason, service.ForkFoldFailureReason)
	}
	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 0 {
		t.Fatalf("expected no pending run after the fold, got %d", n)
	}
}

// Review R1 regression: a comment posted one second after the claim may say
// something the run never read (it could have finished reading by then). Its
// run survives the completion, however short the gap.
func TestForkRunDedup_CommentAfterClaimKeepsItsRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: comment after claim", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '60 seconds'", "now() - interval '59 seconds'")
	second := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '58 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("run of a comment posted after the claim: got status %q, want queued", status)
	}
}

// The same holds for upstream's completion replay: a reply in the run's own
// thread, one second after its claim, is replayed and the replay is kept.
func TestForkRunDedup_ReplayOfCommentAfterClaimIsKept(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: replay", "in_progress", agentID)

	trigger := dbfx.Comment(t, issueID, "initial request", testutil.Cols{
		"created_at": testutil.Raw("now() - interval '61 seconds'"),
	})
	first := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":              runtimeID,
		"issue_id":                issueID,
		"status":                  "running",
		"trigger_comment_id":      trigger,
		"delivered_comment_ids":   testutil.Raw("ARRAY['" + trigger + "'::uuid]"),
		"trigger_evidence_kind":   "comment",
		"trigger_evidence_ref_id": trigger,
		"workflow_step":           "in_progress",
		"created_at":              testutil.Raw("now() - interval '60 seconds'"),
		"dispatched_at":           testutil.Raw("now() - interval '59 seconds'"),
		"started_at":              testutil.Raw("now() - interval '58 seconds'"),
	})
	dbfx.Comment(t, issueID, "and also this", testutil.Cols{
		"parent_id":  trigger,
		"created_at": testutil.Raw("now() - interval '58 seconds'"),
	})

	forkDedupComplete(t, first)

	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 1 {
		t.Fatalf("expected the replayed comment to keep its run, got %d pending runs", n)
	}
}

// A status change between the run's creation and its claim means the claim
// did not render the step the queued run was created under: it is kept.
func TestForkRunDedup_StatusChangeBeforeClaimKeepsRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: status change", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "todo", "now() - interval '60 seconds'", "now() - interval '50 seconds'")
	dbfx.Exec(t, `
		INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{"from":"todo","to":"in_review"}'::jsonb, now() - interval '56 seconds')
	`, testWorkspaceID, issueID, testUserID)
	second := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '55 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("run after a status change before the claim: got status %q, want queued", status)
	}
}

// The fold touches only the completed run's own agent and issue.
func TestForkRunDedup_FoldLeavesOtherAgentsAndIssues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	otherRuntime := dbfx.Runtime(t, "run dedup other runtime")
	otherAgent := dbfx.Agent(t, "run dedup other agent", otherRuntime)
	issueID := forkDedupIssue(t, "run dedup: fold isolation", "in_review", agentID)
	otherIssueID := forkDedupIssue(t, "run dedup: fold isolation, other issue", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '60 seconds'", "now() - interval '50 seconds'")
	otherAgentRun := forkDedupCommentRun(t, otherAgent, otherRuntime, issueID, "now() - interval '55 seconds'")
	otherIssueRun := forkDedupCommentRun(t, agentID, runtimeID, otherIssueID, "now() - interval '55 seconds'")

	forkDedupComplete(t, first)

	for name, id := range map[string]string{"other agent": otherAgentRun, "other issue": otherIssueRun} {
		if status, _ := forkDedupTaskState(t, id); status != "queued" {
			t.Errorf("%s: got status %q, want queued", name, status)
		}
	}
}

// With the flag off nothing is folded, as upstream.
func TestForkRunDedup_FlagOffFoldsNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, false)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: flag off, fold", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "in_review", "now() - interval '60 seconds'", "now() - interval '50 seconds'")
	second := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '55 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("flag off: got status %q, want queued", status)
	}
}

// Scenario 1: the issue is assigned to the agent (its run is claimed at once,
// bound to the current step) and a second later moved into a step that hands
// it to the same agent. The handoff's run replaces the first one.
func TestForkRunDedup_HandoffSupersedesJustClaimedRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: handoff", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "todo", "now() - interval '2 seconds'", "now() - interval '1 second'")
	forkDedupHandoff(t, issueID)

	if status, reason := forkDedupTaskState(t, first); status != "cancelled" || reason != service.ForkSupersedeFailureReason {
		t.Fatalf("first run: got status %q reason %q, want cancelled %q", status, reason, service.ForkSupersedeFailureReason)
	}
	if n := dbfx.Count(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running')`,
		issueID, agentID); n != 1 {
		t.Fatalf("expected exactly one live run, got %d", n)
	}
	if n := dbfx.Count(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND workflow_step = 'in_review'`,
		issueID, agentID); n != 1 {
		t.Fatalf("expected one queued run bound to the new step, got %d", n)
	}
}

// A run claimed before the grace window may already be working; the handoff's
// run follows it as before.
func TestForkRunDedup_HandoffLeavesWorkingRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: handoff, working run", "in_review", agentID)

	first := forkDedupRun(t, agentID, runtimeID, issueID, "todo", "now() - interval '60 seconds'", "now() - interval '59 seconds'")
	forkDedupHandoff(t, issueID)

	if status, _ := forkDedupTaskState(t, first); status != "running" {
		t.Fatalf("working run: got status %q, want running", status)
	}
}

// A handoff that finds the agent's run still queued for the previous step
// rebinds it; the handoff coalesces into it instead of queuing a second run.
func TestForkRunDedup_HandoffRebindsQueuedRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: handoff, queued run", "in_review", agentID)

	queued := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"issue_id":              issueID,
		"status":                "queued",
		"trigger_evidence_kind": "issue_assignment",
		"workflow_step":         "todo",
	})
	forkDedupHandoff(t, issueID)

	var step string
	dbfx.QueryRow(t, `SELECT COALESCE(workflow_step, '') FROM agent_task_queue WHERE id = $1`, queued).Scan(&step)
	if step != "in_review" {
		t.Fatalf("queued run: got step %q, want in_review", step)
	}
	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 1 {
		t.Fatalf("expected the handoff to coalesce into the queued run, got %d pending runs", n)
	}
}
