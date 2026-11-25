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

// forkSupersedeForComment is the hook in triggerTasksForComment, after
// steering and before the comment's runs are enqueued: each agent the comment
// is about to start supersedes its own run claimed moments before the comment
// (service.ForkSupersedeRunsForComment). Squad-leader triggers carry a squad
// context of their own and keep upstream behavior.
func (h *Handler) forkSupersedeForComment(ctx context.Context, issue db.Issue, comment db.Comment, triggers []commentAgentTrigger) {
	if len(triggers) == 0 || h.TaskService == nil || !featureflags.RunDedupV1Enabled(ctx, h.FeatureFlags) {
		return
	}
	for _, trigger := range triggers {
		if trigger.Squad != nil {
			continue
		}
		h.TaskService.ForkSupersedeRunsForComment(ctx, issue.ID, trigger.Agent.ID, comment.ID)
	}
}

// forkFoldCoveredRuns is the hook in CompleteTask, after completion
// reconciliation: queued runs whose every input provably reached the completed
// run are folded into it (service.ForkFoldCoveredRuns).
func (h *Handler) forkFoldCoveredRuns(ctx context.Context, task *db.AgentTaskQueue) {
	if task == nil || h.TaskService == nil || !featureflags.RunDedupV1Enabled(ctx, h.FeatureFlags) {
		return
	}
	h.TaskService.ForkFoldCoveredRuns(ctx, *task)
}
