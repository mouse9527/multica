// Added by the mouse9527/multica fork: one run per operation (M-280).

package handler

import (
	"context"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// forkPrepareHandoffRun is the hook in dispatchIssueRun, before a workflow
// handoff enqueues its agent run: the agent's runs bound to the step the issue
// just left stop competing with it (service.ForkPrepareHandoffRun). A legacy
// handoff note travels only with the new run, so a write carrying one keeps
// upstream behavior.
func (h *Handler) forkPrepareHandoffRun(ctx context.Context, issue db.Issue, trigger service.IssueRunTrigger, handoffNote string) {
	if !trigger.WorkflowHandoff || handoffNote != "" || h.TaskService == nil ||
		!featureflags.RunDedupV1Enabled(ctx, h.FeatureFlags) {
		return
	}
	h.TaskService.ForkPrepareHandoffRun(ctx, issue)
}

// forkFoldCoveredRuns is the hook in CompleteTask, after completion
// reconciliation: queued runs whose every input the completed run could read
// are folded into it (service.ForkFoldCoveredRuns).
func (h *Handler) forkFoldCoveredRuns(ctx context.Context, task *db.AgentTaskQueue) {
	if task == nil || h.TaskService == nil || !featureflags.RunDedupV1Enabled(ctx, h.FeatureFlags) {
		return
	}
	h.TaskService.ForkFoldCoveredRuns(ctx, *task)
}
