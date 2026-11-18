// Modified for the mouse9527/multica fork: Router notices in the Router chat (M-111).

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// PostRouterChatNotice writes a Router system notice (a dispatch pause, a busy
// Router) into the Router's chat as an ordinary assistant message with no
// task, the way the Mika onboarding opening is written, and raises the same
// chat reply alert a finished turn raises (chat:reply_notification), so the
// owner hears about it where they talk to the Router. No run is started and
// the agent never reads the message as input.
//
// The chat must be active and still with agentID. Errors to expect:
// ErrChatSessionArchived, ErrRouterChatUnavailable.
func (s *TaskService) PostRouterChatNotice(ctx context.Context, sessionID, agentID pgtype.UUID, content string) (db.ChatMessage, error) {
	var session db.ChatSession
	var msg db.ChatMessage
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if _, err := qtx.LockChatSessionForRuntimeBind(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the chat no longer exists", ErrRouterChatUnavailable)
			}
			return fmt.Errorf("lock chat session: %w", err)
		}
		var err error
		if session, err = qtx.GetChatSession(ctx, sessionID); err != nil {
			return fmt.Errorf("reload chat session: %w", err)
		}
		if session.Status != "active" {
			return ErrChatSessionArchived
		}
		if session.AgentID != agentID {
			return fmt.Errorf("%w: the chat belongs to another agent", ErrRouterChatUnavailable)
		}
		if msg, err = qtx.CreateChatMessage(ctx, db.CreateChatMessageParams{
			ID:            dbid.NewV7(),
			ChatSessionID: session.ID,
			Role:          "assistant",
			Content:       content,
		}); err != nil {
			return fmt.Errorf("create router chat notice: %w", err)
		}
		return qtx.TouchChatSession(ctx, session.ID)
	})
	if err != nil {
		return db.ChatMessage{}, err
	}

	workspaceID := util.UUIDToString(session.WorkspaceID)
	sessionKey := util.UUIDToString(session.ID)
	ownerID := util.UUIDToString(session.CreatorID)
	messageID := util.UUIDToString(msg.ID)
	s.Bus.Publish(events.Event{
		Type:               protocol.EventChatMessage,
		WorkspaceID:        workspaceID,
		ActorType:          "system",
		ChatSessionID:      sessionKey,
		ChatSessionOwnerID: ownerID,
		Payload: protocol.ChatMessagePayload{
			ChatSessionID: sessionKey,
			MessageID:     messageID,
			Role:          "assistant",
			Content:       msg.Content,
			CreatedAt:     msg.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		},
	})
	title := strings.TrimSpace(session.Title)
	if title == "" {
		title = "Chat reply"
	}
	preview := []rune(strings.Join(strings.Fields(msg.Content), " "))
	if len(preview) > 200 {
		preview = append(preview[:200], '…')
	}
	s.Bus.Publish(events.Event{
		Type:               protocol.EventChatReplyNotification,
		WorkspaceID:        workspaceID,
		ActorType:          "system",
		ChatSessionID:      sessionKey,
		ChatSessionOwnerID: ownerID,
		Payload: protocol.ChatReplyNotificationPayload{
			WorkspaceID: workspaceID, ChatSessionID: sessionKey, MessageID: messageID,
			Title: title, Body: string(preview),
		},
	})
	return msg, nil
}
