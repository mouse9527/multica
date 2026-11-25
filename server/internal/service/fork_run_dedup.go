// Added by the mouse9527/multica fork: one run per operation (M-280).

package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
// Neither is fixed by guessing what the first run read. A run's claim fixes
// its brief and its comment delta; whatever arrives after the claim may or
// may not be read, so it is never counted as covered. Instead:
//
//   - a new trigger that arrives within forkRunClaimGrace of the agent's claim
//     supersedes that run: the run is cancelled and the new trigger's run is
//     claimed after every input exists, so its claim carries all of them. The
//     only cost of a wrong guess about the old run is work it redoes; no input
//     depends on it. On 92 duplicate pairs from our instance the first issue or
//     comment read came 5.1s after the claim at the earliest (median 12s), so
//     within the grace the cancelled run has normally done nothing;
//   - a successful run folds the queued runs whose every input provably
//     reached it: in its delivery receipt, or already on the issue when it was
//     claimed (ForkFoldCoveredQueuedRuns).
const (
	forkRunClaimGrace = 3 * time.Second
	// forkRunFoldLead bounds how long before the covering run was created a
	// folded input may be: inputs of the same operation, not an old backlog.
	forkRunFoldLead = 15 * time.Second

	// Failure reasons recorded on the cancelled run. The web client shows an
	// unknown reason as its raw value; the error text explains it.
	ForkFoldFailureReason      = "folded_into_prior_run"
	ForkSupersedeFailureReason = "superseded_by_newer_trigger"
)

// ForkPrepareHandoffRun runs before a workflow handoff enqueues a run for the
// issue's agent assignee. The agent's runs bound to the step the issue has
// just left stop competing with the handoff's run:
//
//   - a queued run is rebound to the new step. Its claim then renders the new
//     step's brief, and the handoff's own enqueue coalesces into it when the
//     two share a pending slot;
//   - a run claimed within forkRunClaimGrace is superseded by the handoff's
//     run, which is claimed after the run's inputs and the handoff both exist.
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
	s.forkSupersedeRuns(ctx, issue.ID, issue.AssigneeID, stale,
		"Superseded by the handoff to step "+issue.Status+" right after it was claimed; the handoff's run carries this run's inputs.")
}

// ForkSupersedeRunsForComment runs before a comment enqueues a run for
// agentID: the agent's runs on the issue claimed within forkRunClaimGrace
// before the comment was posted, outside any comment thread or in the
// comment's own thread, are superseded by the comment's run. Their claims
// could not include the comment; the comment's run is claimed after it and
// after their inputs. Runs in another thread keep their own reply and are
// left alone. Errors are logged; the comment enqueues either way.
func (s *TaskService) ForkSupersedeRunsForComment(ctx context.Context, issueID, agentID, commentID pgtype.UUID) {
	if s == nil || s.Queries == nil || !issueID.Valid || !agentID.Valid || !commentID.Valid {
		return
	}
	stale, err := s.Queries.ForkListRunsSupersededByComment(ctx, db.ForkListRunsSupersededByCommentParams{
		CommentID:    commentID,
		IssueID:      issueID,
		AgentID:      agentID,
		GraceSeconds: forkRunClaimGrace.Seconds(),
	})
	if err != nil {
		slog.Warn("run dedup: list runs superseded by comment failed",
			"issue_id", util.UUIDToString(issueID), "comment_id", util.UUIDToString(commentID), "error", err)
		return
	}
	s.forkSupersedeRuns(ctx, issueID, agentID, stale,
		"Superseded by comment "+util.UUIDToString(commentID)+" posted right after it was claimed; the comment's run carries this run's inputs.")
}

func (s *TaskService) forkSupersedeRuns(ctx context.Context, issueID, agentID pgtype.UUID, ids []pgtype.UUID, msg string) {
	for _, id := range ids {
		if _, err := s.CancelTaskWithReason(ctx, id, msg, ForkSupersedeFailureReason); err != nil {
			slog.Warn("run dedup: cancel superseded run failed",
				"issue_id", util.UUIDToString(issueID), "task_id", util.UUIDToString(id), "error", err)
			continue
		}
		slog.Info("run dedup: run superseded by a newer trigger",
			"issue_id", util.UUIDToString(issueID),
			"agent_id", util.UUIDToString(agentID),
			"task_id", util.UUIDToString(id))
	}
}

// ForkFoldCoveredRuns runs after a run completed successfully, once completion
// reconciliation has replayed what the run did not receive. Every queued run
// of the same agent on the same issue whose inputs all provably reached the
// completed run is cancelled as folded into it (ForkFoldCoveredQueuedRuns
// lists the conditions). Input posted after the claim always keeps its run.
// Returns the number of runs folded; errors are logged and fold nothing.
func (s *TaskService) ForkFoldCoveredRuns(ctx context.Context, completed db.AgentTaskQueue) int {
	if s == nil || s.Queries == nil || !completed.IssueID.Valid || completed.Status != "completed" {
		return 0
	}
	var folded []db.AgentTaskQueue
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		rows, err := qtx.ForkFoldCoveredQueuedRuns(ctx, db.ForkFoldCoveredQueuedRunsParams{
			FailureReason:   ForkFoldFailureReason,
			LeadSeconds:     forkRunFoldLead.Seconds(),
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
