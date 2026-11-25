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

// forkDedupAssignmentRun is the run an assignment or handoff started: claimed
// 59s ago, running, bound to step.
func forkDedupAssignmentRun(t *testing.T, agentID, runtimeID, issueID, step string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"issue_id":              issueID,
		"status":                "running",
		"trigger_evidence_kind": "issue_assignment",
		"workflow_step":         step,
		"created_at":            testutil.Raw("now() - interval '60 seconds'"),
		"dispatched_at":         testutil.Raw("now() - interval '59 seconds'"),
		"started_at":            testutil.Raw("now() - interval '58 seconds'"),
	})
}

// forkDedupCommentRun is the queued run a top-level comment created at
// createdAt (a SQL expression) earned for agentID.
func forkDedupCommentRun(t *testing.T, agentID, runtimeID, issueID, createdAt string) (taskID, commentID string) {
	t.Helper()
	commentID = dbfx.Comment(t, issueID, "please take it from here", testutil.Cols{
		"created_at": testutil.Raw(createdAt),
	})
	taskID = dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":              runtimeID,
		"issue_id":                issueID,
		"status":                  "queued",
		"trigger_comment_id":      commentID,
		"comment_thread_id":       commentID,
		"trigger_evidence_kind":   "comment",
		"trigger_evidence_ref_id": commentID,
		"created_at":              testutil.Raw(createdAt),
	})
	return taskID, commentID
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

// Scenario 2: the issue moves into the agent's step (its run is claimed at
// once) and a comment mentioning the agent lands a second later. The comment's
// run is folded into the first run when that run completes.
func TestForkRunDedup_CommentRightAfterClaimFoldsIntoRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: comment after claim", "in_review", agentID)

	first := forkDedupAssignmentRun(t, agentID, runtimeID, issueID, "in_review")
	second, _ := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '58 seconds'")

	forkDedupComplete(t, first)

	if status, reason := forkDedupTaskState(t, second); status != "cancelled" || reason != service.ForkFoldFailureReason {
		t.Fatalf("comment run: got status %q reason %q, want cancelled %q", status, reason, service.ForkFoldFailureReason)
	}
	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 0 {
		t.Fatalf("expected no pending run after the fold, got %d", n)
	}
}

// With the flag off the comment's run stays queued, as upstream.
func TestForkRunDedup_FlagOffKeepsUpstreamBehavior(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, false)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: flag off", "in_review", agentID)

	first := forkDedupAssignmentRun(t, agentID, runtimeID, issueID, "in_review")
	second, _ := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '58 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("comment run with the flag off: got status %q, want queued", status)
	}
}

// A comment that arrives after the grace window may not have been read: its
// run is kept.
func TestForkRunDedup_LaterCommentKeepsItsRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: later comment", "in_review", agentID)

	first := forkDedupAssignmentRun(t, agentID, runtimeID, issueID, "in_review")
	// Claimed 59s ago; this comment came 10s after the claim.
	second, _ := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '49 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("run of a comment posted after the window: got status %q, want queued", status)
	}
}

// A comment-triggered run that resumed an earlier one with no new comment
// since may have been told to skip its comment scan, so a comment that came
// after its claim is not counted as read, however soon.
func TestForkRunDedup_SkippedScanKeepsCommentAfterClaim(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: skipped scan", "in_review", agentID)

	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"issue_id":              issueID,
		"status":                "completed",
		"trigger_evidence_kind": "issue_assignment",
		"created_at":            testutil.Raw("now() - interval '10 minutes'"),
		"dispatched_at":         testutil.Raw("now() - interval '10 minutes'"),
		"started_at":            testutil.Raw("now() - interval '10 minutes'"),
		"completed_at":          testutil.Raw("now() - interval '9 minutes'"),
	})
	trigger := dbfx.Comment(t, issueID, "one more thing", testutil.Cols{
		"created_at": testutil.Raw("now() - interval '61 seconds'"),
	})
	first := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":              runtimeID,
		"issue_id":                issueID,
		"status":                  "running",
		"trigger_comment_id":      trigger,
		"comment_thread_id":       trigger,
		"delivered_comment_ids":   testutil.Raw("ARRAY['" + trigger + "'::uuid]"),
		"trigger_evidence_kind":   "comment",
		"trigger_evidence_ref_id": trigger,
		"workflow_step":           "in_review",
		"created_at":              testutil.Raw("now() - interval '60 seconds'"),
		"dispatched_at":           testutil.Raw("now() - interval '59 seconds'"),
		"started_at":              testutil.Raw("now() - interval '58 seconds'"),
	})
	second, _ := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '58 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("comment after a claim that may have skipped the scan: got status %q, want queued", status)
	}
}

// A status change after the first run was created means its claim did not see
// the step the queued run is for: the queued run is kept.
func TestForkRunDedup_StatusChangeInWindowKeepsRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: status change", "in_review", agentID)

	first := forkDedupAssignmentRun(t, agentID, runtimeID, issueID, "todo")
	dbfx.Exec(t, `
		INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{"from":"todo","to":"in_review"}'::jsonb, now() - interval '58 seconds' - interval '500 milliseconds')
	`, testWorkspaceID, issueID, testUserID)
	second, _ := forkDedupCommentRun(t, agentID, runtimeID, issueID, "now() - interval '58 seconds'")

	forkDedupComplete(t, first)

	if status, _ := forkDedupTaskState(t, second); status != "queued" {
		t.Fatalf("run after a status change in the window: got status %q, want queued", status)
	}
}

// The fold touches only the completed run's own agent and issue.
func TestForkRunDedup_OtherAgentsAndIssuesUntouched(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.RunDedupV1, true)
	agentID, runtimeID := forkDedupAgent(t)
	otherRuntime := dbfx.Runtime(t, "run dedup other runtime")
	otherAgent := dbfx.Agent(t, "run dedup other agent", otherRuntime)

	issueID := forkDedupIssue(t, "run dedup: isolation", "in_review", agentID)
	otherIssueID := forkDedupIssue(t, "run dedup: isolation, other issue", "in_review", agentID)

	first := forkDedupAssignmentRun(t, agentID, runtimeID, issueID, "in_review")
	otherAgentRun, _ := forkDedupCommentRun(t, otherAgent, otherRuntime, issueID, "now() - interval '58 seconds'")
	otherIssueRun, _ := forkDedupCommentRun(t, agentID, runtimeID, otherIssueID, "now() - interval '58 seconds'")

	forkDedupComplete(t, first)

	for name, id := range map[string]string{"other agent": otherAgentRun, "other issue": otherIssueRun} {
		if status, _ := forkDedupTaskState(t, id); status != "queued" {
			t.Errorf("%s: got status %q, want queued", name, status)
		}
	}
}

// Upstream #7237: a comment in the run's own thread that arrived right after
// the claim is replayed by completion reconciliation; the replayed run is
// folded too, because the completed run read the comment.
func TestForkRunDedup_ReplayOfCommentReadByRunIsFolded(t *testing.T) {
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
		"comment_thread_id":       trigger,
		"delivered_comment_ids":   testutil.Raw("ARRAY['" + trigger + "'::uuid]"),
		"trigger_evidence_kind":   "comment",
		"trigger_evidence_ref_id": trigger,
		"workflow_step":           "in_progress",
		"created_at":              testutil.Raw("now() - interval '60 seconds'"),
		"dispatched_at":           testutil.Raw("now() - interval '59 seconds'"),
		"started_at":              testutil.Raw("now() - interval '58 seconds'"),
	})
	// A reply in the run's thread, one second after the claim.
	dbfx.Comment(t, issueID, "and also this", testutil.Cols{
		"parent_id":  trigger,
		"created_at": testutil.Raw("now() - interval '58 seconds'"),
	})

	forkDedupComplete(t, first)

	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 0 {
		t.Fatalf("expected the replay to be folded, got %d pending runs", n)
	}
	if n := dbfx.Count(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'cancelled' AND failure_reason = $3`,
		issueID, agentID, service.ForkFoldFailureReason); n != 1 {
		t.Fatalf("expected one folded replay, got %d", n)
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
	ctx := context.Background()
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: handoff", "in_review", agentID)

	first := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"issue_id":              issueID,
		"status":                "running",
		"trigger_evidence_kind": "issue_assignment",
		"workflow_step":         "todo",
		"created_at":            testutil.Raw("now() - interval '2 seconds'"),
		"dispatched_at":         testutil.Raw("now() - interval '1 second'"),
		"started_at":            testutil.Raw("now() - interval '500 milliseconds'"),
	})

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

	if status, reason := forkDedupTaskState(t, first); status != "cancelled" || reason != service.ForkSupersedeFailureReason {
		t.Fatalf("first run: got status %q reason %q, want cancelled %q", status, reason, service.ForkSupersedeFailureReason)
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
	ctx := context.Background()
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: handoff, working run", "in_review", agentID)

	first := forkDedupAssignmentRun(t, agentID, runtimeID, issueID, "todo")

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
	ctx := context.Background()
	agentID, runtimeID := forkDedupAgent(t)
	issueID := forkDedupIssue(t, "run dedup: handoff, queued run", "in_review", agentID)

	queued := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"issue_id":              issueID,
		"status":                "queued",
		"trigger_evidence_kind": "issue_assignment",
		"workflow_step":         "todo",
	})

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

	var step string
	dbfx.QueryRow(t, `SELECT COALESCE(workflow_step, '') FROM agent_task_queue WHERE id = $1`, queued).Scan(&step)
	if step != "in_review" {
		t.Fatalf("queued run: got step %q, want in_review", step)
	}
	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 1 {
		t.Fatalf("expected the handoff to coalesce into the queued run, got %d pending runs", n)
	}
}
