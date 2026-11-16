// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).

package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// EnqueueRouterRun queues a Router anchor run: agentID runs on issueID with
// handoffNote (the batch of captured events) rendered into its prompt. It goes
// through the same path as an explicit mention without a trigger comment, so
// the run gets the usual attribution, project-access and triage guards, and
// the (issue, agent) pending-task index; no human action caused it, so no
// member is passed as the actor.
//
// Errors to expect: ErrDuplicatePendingTask (the agent already has a run
// waiting on the issue), ErrIssueInTriage, ErrProjectAccessDenied,
// ErrAttributionFailClosed, and a plain error for an archived agent or one
// without a runtime.
func (s *TaskService) EnqueueRouterRun(ctx context.Context, issue db.Issue, agentID pgtype.UUID, handoffNote string) (db.AgentTaskQueue, error) {
	return s.enqueueMentionTask(ctx, issue, agentID, pgtype.UUID{}, false, pgtype.UUID{}, false, handoffNote, pgtype.UUID{}, pgtype.UUID{}, OriginDerived)
}
