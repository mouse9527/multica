// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).

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
