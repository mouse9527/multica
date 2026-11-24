// Added by the mouse9527/multica fork: the self-loop brake (M-233).

package agentrouter

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// selfLoopStreak is how many runs in a row, each started only by events
	// the subscribed agent caused, engage the brake.
	selfLoopStreak = 5
	// selfLoopCooldown is how long, once engaged, a batch of only the agent's
	// own events waits after the last run.
	selfLoopCooldown = 15 * time.Minute
)

// selfLoopBrake guards against a Super Agent waking itself. Only its own
// chat's turns and its dispatched runs are its own (ownRunSQL); anything else
// its agent does starts a batch, so a Super Agent write that ever lost its run
// id would wake it again and again. The brake holds back a batch whose
// events its agent caused, every one of them, when the subscription's last
// selfLoopStreak runs were each started by such a batch and the newest
// started less than selfLoopCooldown ago. The events stay pending: they go
// with the next batch, which an event from anyone else starts at once and
// the cooldown starts otherwise. A batch with any event from someone else is
// never held.
//
// The same agent working issues the Super Agent handed it also produces
// agent-only batches; past the streak, its progress reaches the chat at most
// once per cooldown, which is the price of the guard.
//
// It reports whether to hold the batch and, when it does, the id of the
// notice to deliver after the commit (at most one per subscription and hour).
func selfLoopBrake(ctx context.Context, tx pgx.Tx, sub dispatchSub, b batch, agentName, agentOwner string) (bool, string, error) {
	ids := make([]string, len(b.events))
	for i, e := range b.events {
		ids[i] = e.ID
	}
	var held bool
	err := tx.QueryRow(ctx, `
		WITH recent AS (
		    SELECT d.task_id, d.created_at FROM router_dispatch d
		    WHERE d.subscription_id = $1::uuid
		    ORDER BY d.created_at DESC
		    LIMIT $4
		),
		judged AS (
		    SELECT r.created_at,
		           EXISTS (SELECT 1 FROM router_event e
		                   WHERE e.subscription_id = $1::uuid AND e.consumed_task_id = r.task_id AND NOT e.is_self)
		           AND NOT EXISTS (SELECT 1 FROM router_event e
		                   WHERE e.subscription_id = $1::uuid AND e.consumed_task_id = r.task_id AND NOT e.is_self
		                     AND NOT (e.actor_type = 'agent' AND e.actor_id = $2::uuid)) AS agent_only
		    FROM recent r
		)
		SELECT NOT EXISTS (
		           SELECT 1 FROM router_event e
		           WHERE e.id = ANY($3::uuid[]) AND NOT (e.actor_type = 'agent' AND e.actor_id = $2::uuid))
		       AND (SELECT count(*) FROM judged WHERE agent_only) = $4
		       AND (SELECT max(created_at) FROM judged) > now() - make_interval(secs => $5)`,
		sub.ID, sub.AgentID, ids, selfLoopStreak, selfLoopCooldown.Seconds(),
	).Scan(&held)
	if err != nil || !held {
		return false, "", err
	}
	recipient := noticeRecipient(sub, agentOwner)
	if recipient == "" {
		return true, "", nil
	}
	n, created, err := insertNotice(ctx, tx, NoticeInput{
		WorkspaceID:     sub.WorkspaceID,
		RecipientUserID: recipient,
		Title:           fmt.Sprintf("Super Agent may be waking itself: %s", agentName),
		Body: fmt.Sprintf("Super Agent subscription %s started its last %d runs for events that only its own agent %s caused. "+
			"From now on, events only that agent causes wait %s after the last run, or go with the next event someone else causes. "+
			"Nothing is lost and nothing is paused. If its own chat turns are waking it, look at `multica router events --subscription %s`; "+
			"stop it with `multica router pause %s`.",
			sub.ID, selfLoopStreak, agentName, selfLoopCooldown, sub.ID, sub.ID),
		Refs:                 anchorRefs(sub),
		Urgency:              UrgencyNormal,
		DedupeKey:            fmt.Sprintf("router_self_loop:%s:%s", sub.ID, time.Now().UTC().Format("2006010215")),
		SenderType:           "system",
		SourceSubscriptionID: sub.ID,
	})
	if err != nil || !created {
		return true, "", err
	}
	return true, n.ID, nil
}
