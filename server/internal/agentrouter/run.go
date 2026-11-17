// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), Router v6 slice 1 (M-96), and Router notices in the Router chat (M-111).

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

// PostRouterChatNotice writes a Router system notice into the chat (see
// service.PostRouterChatNotice).
func (r TaskRuns) PostRouterChatNotice(ctx context.Context, chatSessionID, agentID, content string) (string, error) {
	sessionUUID, err := util.ParseUUID(chatSessionID)
	if err != nil {
		return "", err
	}
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return "", err
	}
	msg, err := r.Tasks.PostRouterChatNotice(ctx, sessionUUID, agentUUID, content)
	if errors.Is(err, service.ErrChatSessionArchived) || errors.Is(err, service.ErrRouterChatUnavailable) {
		return "", fmt.Errorf("%w: %v", ErrChatUnavailable, err)
	}
	if err != nil {
		return "", err
	}
	return util.UUIDToString(msg.ID), nil
}

// EnqueueRouterChatRun writes the batch into the chat session and queues the
// chat turn that reads it (see service.EnqueueRouterChatRun).
func (r TaskRuns) EnqueueRouterChatRun(ctx context.Context, chatSessionID, agentID, anchorIssueID, batch string) (string, error) {
	sessionUUID, err := util.ParseUUID(chatSessionID)
	if err != nil {
		return "", err
	}
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return "", err
	}
	anchorUUID, err := util.ParseUUID(anchorIssueID)
	if err != nil {
		return "", err
	}
	task, err := r.Tasks.EnqueueRouterChatRun(ctx, sessionUUID, agentUUID, anchorUUID, batch)
	switch {
	case errors.Is(err, service.ErrChatSessionArchived), errors.Is(err, service.ErrRouterChatUnavailable):
		return "", fmt.Errorf("%w: %v", ErrChatUnavailable, err)
	case errors.Is(err, service.ErrChatTaskAgentArchived), errors.Is(err, service.ErrChatTaskAgentNoRuntime),
		errors.Is(err, service.ErrAttributionFailClosed), errors.Is(err, service.ErrProjectAccessDenied):
		return "", fmt.Errorf("%w: %v", ErrRunRefused, err)
	case err != nil:
		return "", err
	}
	return util.UUIDToString(task.ID), nil
}
