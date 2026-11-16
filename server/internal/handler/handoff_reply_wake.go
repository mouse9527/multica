// Modified for the mouse9527/multica fork: handoff reply wake (M-89).

package handler

import (
	"context"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// computeCommentAgentTriggers is the fork's wrapper around the upstream trigger
// resolution (computeUpstreamCommentAgentTriggers). It is the only entry point,
// so comment create, preview, edit re-trigger and the daemon path all agree.
func (h *Handler) computeCommentAgentTriggers(ctx context.Context, issue db.Issue, content string, parentComment *db.Comment, actorType, actorID string, opts commentTriggerComputeOptions) ([]commentAgentTrigger, []commentMentionTarget) {
	triggers, targets := h.computeUpstreamCommentAgentTriggers(ctx, issue, content, parentComment, actorType, actorID, opts)
	// Same thread resolution the upstream function applies to its own copy of
	// opts; the assignee route keys its pending-task dedup on it.
	opts.ThreadCommentID = opts.ExcludeTriggerCommentID
	if !opts.ThreadCommentID.Valid && parentComment != nil {
		opts.ThreadCommentID = parentComment.ID
	}
	return h.addHandoffReplyWake(ctx, issue, content, actorType, actorID, opts, triggers), targets
}

// addHandoffReplyWake adds the issue's assignee to triggers when a member
// comments while the issue waits on a person (issuestatus.IsHandoff).
//
// Upstream routes a reply only to the thread it answers, so a member answering
// a decision card inside another agent's thread never reached the assignee who
// was waiting for that answer. Behind handoff_reply_wake, any member comment
// in a handoff status also wakes the assignee; the upstream targets stay as
// they are and an assignee already among them is not added twice. Agent
// comments never take this route, which keeps agents from waking each other in
// a loop. A status that cannot be resolved falls back to upstream behavior.
func (h *Handler) addHandoffReplyWake(ctx context.Context, issue db.Issue, content, actorType, actorID string, opts commentTriggerComputeOptions, triggers []commentAgentTrigger) []commentAgentTrigger {
	if actorType != "member" || isNoteComment(content) {
		return triggers
	}
	if !featureflags.HandoffReplyWakeEnabled(ctx, h.FeatureFlags) {
		return triggers
	}
	handoff, err := issuestatus.IsHandoff(ctx, h.Queries, issue.WorkspaceID, issue.Status)
	if err != nil || !handoff {
		return triggers
	}
	assignee, ok := h.routeAssigneeFallback(ctx, issue, actorType, actorID, opts)
	if !ok {
		return triggers
	}
	for _, t := range triggers {
		if t.Agent.ID == assignee.Agent.ID {
			return triggers
		}
	}
	return append(triggers, assignee)
}
