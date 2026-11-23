// Added by the mouse9527/multica fork: name the run an agent acted from on
// issue events (M-193).
//
// A Super Agent leaves its own actions out of its batches. Its agent can also
// work issue tasks, and those actions must reach the batch, so the capture
// needs the run each action came from, not only the agent.

package handler

import (
	"net/http"

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

// publishWithActorTask is publish plus the run the actor acted from.
func (h *Handler) publishWithActorTask(r *http.Request, eventType, workspaceID, actorType, actorID string, payload any) {
	h.Bus.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: workspaceID,
		ActorType:   actorType,
		ActorID:     actorID,
		ActorTaskID: actorTaskID(r, actorType),
		Payload:     payload,
	})
}
