// Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96).

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// RouterChatPriority ranks a Router batch turn below a member's chat turn
// (priority 2), so a queued member message is claimed before a queued batch
// in the same session. A running batch is not interrupted.
const RouterChatPriority = 1

// routerChatMaxAttempts turns the system's auto-retry off for a batch turn:
// a chat retry is raised to priority 3 (CreateRetryTask) and would then be
// claimed before the member's queued messages. A batch whose turn fails is
// not lost: Router dispatch settles the failed turn, puts its events back in
// the queue, and writes them into the chat again as a new batch at
// RouterChatPriority.
const routerChatMaxAttempts = 1

// ErrRouterChatUnavailable: the chat cannot take a batch until someone
// changes something (it is gone, or now belongs to another agent).
// ErrChatSessionArchived covers an archived chat.
var ErrRouterChatUnavailable = errors.New("router chat: session unavailable")

// EnqueueRouterChatRun writes a Router batch into a chat session and queues
// the chat turn that answers it, in one transaction, the way a member's send
// does: lock the session, create the task, make it own its input, write the
// input. The input is a user-role message of kind router_batch whose content
// is batch, so the claim delivers it like any chat input and the daemon needs
// no change; batch must say on its own that no person wrote it, because the
// claim joins inputs without authors.
//
// No member acted, so the run has no originator (an agent acting in it holds
// no member's authority); the agent owner is accountable through the usual
// owner fallback. The session's creator is recorded as initiator because the
// turn belongs to their conversation.
//
// The private-project fence of an anchor run (guardRunProjectAccess on the
// anchor issue, MUL-8472) applies unchanged, before anything is written: the
// batch carries the same issue titles and comment payloads. The member it is
// checked for is the session's creator, who reads the chat, together with the
// agent's owner, whose runtime runs the turn.
//
// Errors to expect: ErrChatSessionArchived, ErrRouterChatUnavailable,
// ErrChatTaskAgentArchived, ErrChatTaskAgentNoRuntime, ErrAttributionFailClosed,
// ErrProjectAccessDenied.
func (s *TaskService) EnqueueRouterChatRun(ctx context.Context, sessionID, agentID, anchorIssueID pgtype.UUID, batch string) (db.AgentTaskQueue, error) {
	session, err := s.Queries.GetChatSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, fmt.Errorf("%w: the chat no longer exists", ErrRouterChatUnavailable)
	}
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load chat session: %w", err)
	}
	if session.AgentID != agentID {
		return db.AgentTaskQueue{}, fmt.Errorf("%w: the chat belongs to another agent", ErrRouterChatUnavailable)
	}
	agent, err := s.Queries.GetAgent(ctx, session.AgentID)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load chat agent: %w", err)
	}
	attr, err := s.applyAttributionFallback(ctx, attribution.Unattributed(attribution.EvidenceChat, session.ID), agent)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	anchor, err := s.Queries.GetIssue(ctx, anchorIssueID)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load anchor issue: %w", err)
	}
	if err := s.guardRunProjectAccess(ctx, anchor, agent, session.CreatorID, attr.AccountableUserID); err != nil {
		return db.AgentTaskQueue{}, err
	}
	attrSource, _, attrEvidenceKind, attrEvidenceRef := attributionCreateParams(attr)
	overlay := s.buildRuntimeMCPOverlay(ctx, attr.UserID, agent)

	var task db.AgentTaskQueue
	var msg db.ChatMessage
	if s.TxStarter == nil {
		return db.AgentTaskQueue{}, errors.New("router chat enqueue: transaction starter is required")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("begin router chat enqueue: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := func(qtx *db.Queries) error {
		// Same locks, in the same order, as a member's send.
		if _, err := qtx.LockChatSessionForRuntimeBind(ctx, session.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the chat no longer exists", ErrRouterChatUnavailable)
			}
			return fmt.Errorf("lock chat session: %w", err)
		}
		current, err := qtx.GetChatSession(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("reload chat session: %w", err)
		}
		if current.Status != "active" {
			return ErrChatSessionArchived
		}
		carrier, err := qtx.GetAgentForClaimUpdate(ctx, session.AgentID)
		if err != nil {
			return fmt.Errorf("reload chat agent: %w", err)
		}
		if carrier.ArchivedAt.Valid {
			return ErrChatTaskAgentArchived
		}
		if !carrier.RuntimeID.Valid {
			return ErrChatTaskAgentNoRuntime
		}
		task, err = qtx.CreateChatTask(ctx, db.CreateChatTaskParams{
			ID:                   dbid.NewV7(),
			AgentID:              session.AgentID,
			RuntimeID:            carrier.RuntimeID,
			Priority:             RouterChatPriority,
			ChatSessionID:        session.ID,
			InitiatorUserID:      session.CreatorID,
			OriginatorUserID:     attr.UserID,
			AccountableUserID:    attr.AccountableUserID,
			ForceFreshSession:    pgtype.Bool{Bool: false, Valid: true},
			RuntimeMcpOverlay:    overlay.Overlay,
			RuntimeConnectedApps: overlay.ConnectedApps,
			OriginatorSource:     attrSource,
			TriggerEvidenceKind:  attrEvidenceKind,
			TriggerEvidenceRefID: attrEvidenceRef,
		})
		if err != nil {
			return fmt.Errorf("create router chat task: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET max_attempts = $2 WHERE id = $1`, task.ID, routerChatMaxAttempts); err != nil {
			return fmt.Errorf("disable router chat task retries: %w", err)
		}
		task, err = qtx.SetChatTaskInputOwnerSelf(ctx, task.ID)
		if err != nil {
			return fmt.Errorf("stamp router chat input owner: %w", err)
		}
		msg, err = qtx.CreateChatMessage(ctx, db.CreateChatMessageParams{
			ID:            dbid.NewV7(),
			ChatSessionID: session.ID,
			Role:          "user",
			Content:       batch,
			TaskID:        task.ID,
			MessageKind:   pgtype.Text{String: protocol.ChatMessageKindRouterBatch, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("create router batch message: %w", err)
		}
		return qtx.TouchChatSession(ctx, session.ID)
	}(s.Queries.WithTx(tx)); err != nil {
		return db.AgentTaskQueue{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("commit router chat enqueue: %w", err)
	}

	slog.Info("router chat task enqueued",
		"task_id", util.UUIDToString(task.ID),
		"chat_session_id", util.UUIDToString(session.ID),
		"agent_id", util.UUIDToString(session.AgentID))
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
	// The event carries no message id on purpose: clients write a user row
	// from a chat:message that has one, without its kind, and would show the
	// batch as the member's own bubble until the refetch it also triggers.
	sessionKey := util.UUIDToString(session.ID)
	s.Bus.Publish(events.Event{
		Type:               protocol.EventChatMessage,
		WorkspaceID:        util.UUIDToString(session.WorkspaceID),
		ActorType:          "system",
		ChatSessionID:      sessionKey,
		ChatSessionOwnerID: util.UUIDToString(session.CreatorID),
		Payload: protocol.ChatMessagePayload{
			ChatSessionID: sessionKey,
			Role:          "user",
			TaskID:        util.UUIDToString(task.ID),
			CreatedAt:     msg.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		},
	})
	return task, nil
}
