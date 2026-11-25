// Added by the mouse9527/multica fork: one run per operation (M-280).

package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// One operation used to start two runs of the same agent on the same issue:
//
//   - an actor assigns the issue and, a moment later in a second request,
//     moves it into a workflow step that hands it to the same agent. The
//     assignment's run is claimed at once, bound to the previous step; the
//     handoff then queues a second run bound to the new step. The first run
//     works under the wrong brief and its status changes are refused, so the
//     second exists only to redo the move;
//   - an actor moves the issue into a step (its run is claimed at once) and,
//     in the same second, posts a comment mentioning the agent. The comment
//     takes its own pending slot (its thread), so a second run queues behind
//     the first and, when it finally starts, finds nothing left to do.
//
// Both are decided against the moment a run is claimed. Up to then the run's
// brief can still change; from then on the brief is fixed, but the agent only
// reads the issue and its comments once it is running. Measured on the 92
// duplicate pairs in our workspace, the first issue or comment read came 5.1s
// after the claim at the earliest (median 12s), so input that arrived within
// forkRunClaimGrace of the claim is input the run reads.
const (
	forkRunClaimGrace = 3 * time.Second
	// forkRunFoldLead admits inputs from shortly before the covering run was
	// created, such as a comment posted just before the status change whose
	// run then claims. It is the window the duplicate statistics used.
	forkRunFoldLead = 15 * time.Second

	// Failure reasons recorded on the cancelled run. The web client shows an
	// unknown reason as its raw value; the error text explains it.
	ForkFoldFailureReason      = "folded_into_prior_run"
	ForkSupersedeFailureReason = "superseded_by_handoff"
)

// ForkPrepareHandoffRun runs before a workflow handoff enqueues a run for the
// issue's agent assignee. The agent's runs bound to the step the issue has
// just left stop competing with the handoff's run:
//
//   - a queued run is rebound to the new step. Its claim then renders the new
//     step's brief, and the handoff's own enqueue coalesces into it when the
//     two share a pending slot;
//   - a run claimed within forkRunClaimGrace is cancelled. It has not read
//     anything yet, and the handoff's run, bound to the new step, replaces it.
//     Inputs it carried (a trigger comment) are on the issue before the
//     replacement is claimed, so the replacement reads them.
//
// A run claimed earlier than that is left alone: it may already be working,
// and the handoff's run follows it as before. Errors are logged; the handoff
// enqueues either way.
func (s *TaskService) ForkPrepareHandoffRun(ctx context.Context, issue db.Issue) {
	if s == nil || s.Queries == nil || !issue.AssigneeID.Valid || issue.AssigneeType.String != "agent" {
		return
	}
	rebound, err := s.Queries.ForkRebindQueuedRunsToStep(ctx, db.ForkRebindQueuedRunsToStepParams{
		Status:  issue.Status,
		IssueID: issue.ID,
		AgentID: issue.AssigneeID,
	})
	if err != nil {
		slog.Warn("run dedup: rebind queued runs to handoff step failed",
			"issue_id", util.UUIDToString(issue.ID), "error", err)
	} else if len(rebound) > 0 {
		slog.Info("run dedup: queued runs rebound to handoff step",
			"issue_id", util.UUIDToString(issue.ID),
			"agent_id", util.UUIDToString(issue.AssigneeID),
			"step", issue.Status,
			"count", len(rebound))
	}
	stale, err := s.Queries.ForkListSupersededStepRuns(ctx, db.ForkListSupersededStepRunsParams{
		IssueID:      issue.ID,
		AgentID:      issue.AssigneeID,
		GraceSeconds: forkRunClaimGrace.Seconds(),
		Status:       issue.Status,
	})
	if err != nil {
		slog.Warn("run dedup: list runs superseded by handoff failed",
			"issue_id", util.UUIDToString(issue.ID), "error", err)
		return
	}
	for _, id := range stale {
		msg := "Superseded by the handoff to step " + issue.Status +
			" before it read the issue; that step's run replaces it."
		if _, err := s.CancelTaskWithReason(ctx, id, msg, ForkSupersedeFailureReason); err != nil {
			slog.Warn("run dedup: cancel run superseded by handoff failed",
				"issue_id", util.UUIDToString(issue.ID), "task_id", util.UUIDToString(id), "error", err)
			continue
		}
		slog.Info("run dedup: run superseded by handoff",
			"issue_id", util.UUIDToString(issue.ID),
			"agent_id", util.UUIDToString(issue.AssigneeID),
			"task_id", util.UUIDToString(id),
			"step", issue.Status)
	}
}

// ForkFoldCoveredRuns runs after a run completed successfully, once completion
// reconciliation has replayed what the run did not receive. Every queued run
// of the same agent on the same issue whose inputs the completed run could
// all read is cancelled as folded into it (ForkFoldCoveredQueuedRuns lists the
// conditions). A replayed comment that arrived within the grace window is
// folded the same way, which is how a comment the run already read stops
// earning a second run. Input that arrived later, or a change to the issue's
// step or body after the run was created, keeps its run. Returns the number
// of runs folded; errors are logged and fold nothing.
func (s *TaskService) ForkFoldCoveredRuns(ctx context.Context, completed db.AgentTaskQueue) int {
	if s == nil || s.Queries == nil || !completed.IssueID.Valid || completed.Status != "completed" {
		return 0
	}
	var folded []db.AgentTaskQueue
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		rows, err := qtx.ForkFoldCoveredQueuedRuns(ctx, db.ForkFoldCoveredQueuedRunsParams{
			FailureReason:   ForkFoldFailureReason,
			LeadSeconds:     forkRunFoldLead.Seconds(),
			GraceSeconds:    forkRunClaimGrace.Seconds(),
			CompletedTaskID: completed.ID,
		})
		if err != nil {
			return err
		}
		// Every terminal write settles supplements and recovery receipts in
		// the same transaction (see settleTerminalTaskState). A folded run was
		// never claimed, so both are normally empty and nothing is requeued.
		if _, err := settleTerminalTaskState(ctx, qtx, rows...); err != nil {
			return err
		}
		folded = rows
		return nil
	})
	if err != nil {
		slog.Warn("run dedup: fold covered queued runs failed",
			"task_id", util.UUIDToString(completed.ID), "error", err)
		return 0
	}
	for _, task := range folded {
		slog.Info("run dedup: queued run folded into completed run",
			"issue_id", util.UUIDToString(task.IssueID),
			"agent_id", util.UUIDToString(task.AgentID),
			"task_id", util.UUIDToString(task.ID),
			"covering_task_id", util.UUIDToString(completed.ID))
		s.captureTaskCancelled(ctx, task)
		s.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
		s.NotifyTaskFinished(task)
	}
	if len(folded) > 0 {
		s.ReconcileAgentStatus(ctx, completed.AgentID)
	}
	return len(folded)
}
