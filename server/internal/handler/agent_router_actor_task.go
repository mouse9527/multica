// Added by the mouse9527/multica fork: name the run an agent acted from on
// issue events (M-193) and on quick-action comments (M-233).
// Modified for the mouse9527/multica fork: private blocker redaction (M-277).
//
// A Super Agent leaves its own actions out of its batches. Its agent can also
// work issue tasks, and those actions must reach the batch, so the capture
// needs the run each action came from, not only the agent.

package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
)

// actorTaskID is the run an agent request acts from. For an agent actor the
// X-Task-ID header is server-trusted: the auth middleware stamps it from the
// task token, and resolveActor checked the task belongs to the agent.
func actorTaskID(r *http.Request, actorType string) string {
	if actorType != "agent" {
		return ""
	}
	return r.Header.Get("X-Task-ID")
}

// publishWithActorTask is publish plus the run the actor acted from. It
// strips private blocker references from an issue payload exactly as publish
// does (TestPublishWithActorTaskRedactsLikePublish keeps the two in step).
func (h *Handler) publishWithActorTask(r *http.Request, eventType, workspaceID, actorType, actorID string, payload any) {
	if fields, ok := payload.(map[string]any); ok {
		if issue, ok := fields["issue"].(IssueResponse); ok {
			fields["issue"] = publicBlockerResponse(issue)
		}
	}
	h.Bus.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: workspaceID,
		ActorType:   actorType,
		ActorID:     actorID,
		ActorTaskID: actorTaskID(r, actorType),
		Payload:     payload,
	})
}

// quickActionSourceTask is the run an agent ran a quick action from, recorded
// as the comment's source_task_id the way CreateComment records it; none for
// a member. Without it a Super Agent's own quick action would have an unknown
// run and wake its own Super Agent (M-233).
//
// The task must be the acting agent's: a run id on the comment makes the
// comment that run's, and a wrong one would silence it as the Super Agent's
// own. resolveActor already ties X-Task-ID to the agent for RunQuickAction;
// the check keeps that true for any other caller.
func quickActionSourceTask(h *Handler, r *http.Request, actorType, actorID string) pgtype.UUID {
	if actorType != "agent" {
		return pgtype.UUID{}
	}
	task, ok := h.taskFromRequestHeader(r)
	if !ok || uuidToString(task.AgentID) != actorID {
		return pgtype.UUID{}
	}
	return task.ID
}
