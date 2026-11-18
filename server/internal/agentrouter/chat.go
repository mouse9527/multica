// Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96), and Router parallelism (M-104).

package agentrouter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// A subscription bound to a chat session (router_chat_anchor) is answered in
// that chat: with router_chat on, dispatch writes each batch into the chat as
// a router_batch message and queues the chat turn that reads it, instead of
// an anchor run. The Router then talks to the owner in the same conversation
// the owner talks to it in. Batching, the hourly alert, pauses and state are
// the same as for anchor runs; the dispatch row still names the anchor issue,
// and its task is the chat turn. The chat is the Router's unit of order: one
// batch turn at a time per chat, from whichever subscriptions are bound to it,
// while other chats of the same agent run in parallel (see Dispatcher).

// ErrChatUnavailable: the bound chat cannot take a batch until someone
// changes something (archived, deleted, now another agent's). Dispatch pauses
// and tells the owner through the inbox, since the chat is no longer a way to
// reach them.
var ErrChatUnavailable = errors.New("router: the bound chat is unavailable")

// RouterBatchLabel opens every batch written into a chat. The claim hands the
// agent its chat inputs joined without authors, so the batch has to say on
// its own that the owner did not write it.
const RouterBatchLabel = "[Router 事件批次 · 非 Owner 发言]"

// subscriptionOwnerSQL is the member a subscription answers to: its owner,
// or the subscribed agent's owner for a subscription without one (an agent
// that subscribed itself). Notices go to the same member.
const subscriptionOwnerSQL = `COALESCE(s.owner_user_id, (SELECT a.owner_id FROM agent a WHERE a.id = s.agent_id))`

// setChatAnchor binds sub to chatSessionID, or removes the binding when
// chatSessionID is empty. The chat must be an active chat in the
// subscription's workspace, with the subscribed agent, started by the
// subscription's owner: the batches carry the scope's activity into that
// member's conversation, and only its creator can read a chat. The
// subscription must keep an anchor issue, which dispatch rows still name.
func setChatAnchor(ctx context.Context, tx pgx.Tx, sub Subscription, anchorIssueID, chatSessionID string) error {
	if chatSessionID == "" {
		_, err := tx.Exec(ctx, `DELETE FROM router_chat_anchor WHERE subscription_id = $1::uuid`, sub.ID)
		return err
	}
	if anchorIssueID == "" {
		return fmt.Errorf("%w: a chat-bound subscription still needs an anchor issue; set one with --anchor", ErrInvalid)
	}
	var agentID, creatorID, status, owner string
	err := tx.QueryRow(ctx, `
		SELECT cs.agent_id::text, cs.creator_id::text, cs.status,
		       COALESCE((SELECT `+subscriptionOwnerSQL+`::text FROM router_subscription s WHERE s.id = $3::uuid), '')
		FROM chat_session cs
		WHERE cs.id = $1::uuid AND cs.workspace_id = $2::uuid`, chatSessionID, sub.WorkspaceID, sub.ID,
	).Scan(&agentID, &creatorID, &status, &owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: chat session %s not found in this workspace", ErrInvalid, chatSessionID)
	case err != nil:
		return err
	case agentID != sub.AgentID:
		return fmt.Errorf("%w: the chat is with another agent; bind a chat with the subscribed agent", ErrInvalid)
	case creatorID != owner:
		return fmt.Errorf("%w: the chat was started by someone other than the subscription's owner", ErrInvalid)
	case status != "active":
		return fmt.Errorf("%w: the chat is archived", ErrInvalid)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO router_chat_anchor (subscription_id, workspace_id, chat_session_id)
		VALUES ($1::uuid, $2::uuid, $3::uuid)
		ON CONFLICT (subscription_id) DO UPDATE SET chat_session_id = EXCLUDED.chat_session_id, updated_at = now()`,
		sub.ID, sub.WorkspaceID, chatSessionID)
	return err
}

// chatAnchorOf returns the chat session a subscription is bound to, or "".
func chatAnchorOf(ctx context.Context, q Querier, subscriptionID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT chat_session_id::text FROM router_chat_anchor WHERE subscription_id = $1::uuid`, subscriptionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// unusableChat is a dispatching subscription whose bound chat can no longer
// take batches.
type unusableChat struct {
	SubscriptionID string
	ChatSessionID  string
	Why            string
}

// unusableChats finds dispatching subscriptions whose chat was deleted or
// archived, moved to another agent, or whose creator left the workspace.
func (d *Dispatcher) unusableChats(ctx context.Context, workspaceID string) ([]unusableChat, error) {
	rows, err := d.Store.DB.Query(ctx, `
		SELECT s.id::text, ca.chat_session_id::text,
		       CASE WHEN cs.id IS NULL THEN 'was deleted'
		            WHEN cs.status <> 'active' THEN 'was archived'
		            WHEN cs.agent_id <> s.agent_id THEN 'is with another agent now'
		            ELSE 'belongs to someone who is no longer a member of the workspace' END
		FROM router_chat_anchor ca
		JOIN router_subscription s ON s.id = ca.subscription_id
		LEFT JOIN chat_session cs ON cs.id = ca.chat_session_id
		WHERE s.enabled AND s.paused_reason IS NULL
		  AND ($1 = '' OR s.workspace_id = NULLIF($1, '')::uuid)
		  AND (cs.id IS NULL OR cs.status <> 'active' OR cs.agent_id <> s.agent_id
		       OR NOT EXISTS (SELECT 1 FROM member m WHERE m.workspace_id = cs.workspace_id AND m.user_id = cs.creator_id))
		ORDER BY s.id
		LIMIT $2`, workspaceID, maxDueSubscriptions)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (unusableChat, error) {
		var c unusableChat
		err := r.Scan(&c.SubscriptionID, &c.ChatSessionID, &c.Why)
		return c, err
	})
}

// pauseUnusableChats pauses dispatch for every subscription whose chat can
// no longer take batches, so the owner hears about it from the inbox right
// away rather than at the next batch. It returns how many it paused.
func (d *Dispatcher) pauseUnusableChats(ctx context.Context, workspaceID string) (int, error) {
	found, err := d.unusableChats(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	paused := 0
	var errs []error
	for _, c := range found {
		notice, ok, err := d.pauseForChat(ctx, c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			paused++
		}
		if notice != "" && d.Notices != nil {
			d.Notices.Deliver(ctx, notice)
		}
	}
	return paused, errors.Join(errs...)
}

func (d *Dispatcher) pauseForChat(ctx context.Context, c unusableChat) (string, bool, error) {
	tx, err := d.Store.Tx.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	sub, err := lockSubscription(ctx, tx, c.SubscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !sub.Enabled || sub.PausedReason != nil {
		return "", false, nil
	}
	if bound, err := chatAnchorOf(ctx, tx, sub.ID); err != nil || bound != c.ChatSessionID {
		// Rebound or unbound since the scan.
		return "", false, err
	}
	var agentName, agentOwner string
	if err := tx.QueryRow(ctx, `
		SELECT name, COALESCE(owner_id::text, '') FROM agent WHERE id = $1::uuid`, sub.AgentID,
	).Scan(&agentName, &agentOwner); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	notice, err := d.pauseDispatch(ctx, tx, sub, agentName, agentOwner, PausedAgentUnavailable, chatPauseWhy(sub.ID, c.ChatSessionID, c.Why))
	if err != nil {
		return "", false, err
	}
	return notice, true, tx.Commit(ctx)
}

func chatPauseWhy(subscriptionID, chatSessionID, what string) string {
	return fmt.Sprintf("its Router chat %s %s. Bind an active chat with `multica router update %s --anchor-chat <session>`, "+
		"or go back to anchor runs with `--anchor-chat none`, then resume", chatSessionID, what, subscriptionID)
}
