// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), and Router v6 slice 1 (M-96).

package agentrouter

import (
	"context"
	"errors"
	"fmt"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// TaskRuns starts anchor runs through the task service, the same path a
// mention without a trigger comment takes (see service.EnqueueRouterRun), so
// the daemon renders the batch note without any change on its side.
type TaskRuns struct {
	Tasks *service.TaskService
}

func (r TaskRuns) EnqueueRouterRun(ctx context.Context, issueID, agentID, note string) (string, error) {
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return "", err
	}
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return "", err
	}
	issue, err := r.Tasks.Queries.GetIssue(ctx, issueUUID)
	if err != nil {
		return "", fmt.Errorf("load anchor issue: %w", err)
	}
	task, err := r.Tasks.EnqueueRouterRun(ctx, issue, agentUUID, note)
	switch {
	case errors.Is(err, service.ErrDuplicatePendingTask), errors.Is(err, service.ErrIssueInTriage):
		return "", fmt.Errorf("%w: %v", ErrRunBusy, err)
	case errors.Is(err, service.ErrProjectAccessDenied), errors.Is(err, service.ErrAttributionFailClosed):
		return "", fmt.Errorf("%w: %v", ErrRunRefused, err)
	case err != nil:
		return "", err
	}
	return util.UUIDToString(task.ID), nil
}

// EnqueueRouterChatRun writes the batch into the chat session and queues the
// chat turn that reads it (see service.EnqueueRouterChatRun).
func (r TaskRuns) EnqueueRouterChatRun(ctx context.Context, chatSessionID, agentID, batch string) (string, error) {
	sessionUUID, err := util.ParseUUID(chatSessionID)
	if err != nil {
		return "", err
	}
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return "", err
	}
	task, err := r.Tasks.EnqueueRouterChatRun(ctx, sessionUUID, agentUUID, batch)
	switch {
	case errors.Is(err, service.ErrChatSessionArchived), errors.Is(err, service.ErrRouterChatUnavailable):
		return "", fmt.Errorf("%w: %v", ErrChatUnavailable, err)
	case errors.Is(err, service.ErrChatTaskAgentArchived), errors.Is(err, service.ErrChatTaskAgentNoRuntime),
		errors.Is(err, service.ErrAttributionFailClosed):
		return "", fmt.Errorf("%w: %v", ErrRunRefused, err)
	case err != nil:
		return "", err
	}
	return util.UUIDToString(task.ID), nil
}
