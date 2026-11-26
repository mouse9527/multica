// This file belongs to the mouse9527/multica fork (not present upstream).
// M-312 / mouse9527/multica#209.

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// restoreGaveUpChatDraft runs restoreChatDraftAfterAPIRetryGaveUp in its own
// transaction, after the task failure has already been committed. The failure
// therefore stands whatever happens here, and an error is the caller's to log
// rather than propagate: a member who does not get the composer filled keeps
// the prompt as a sent message.
func (s *TaskService) restoreGaveUpChatDraft(ctx context.Context, task db.AgentTaskQueue, errMsg string) (*protocol.ChatCancelFinalizedPayload, error) {
	var payload *protocol.ChatCancelFinalizedPayload
	if err := s.runInTx(ctx, func(qtx *db.Queries) error {
		var err error
		payload, err = restoreChatDraftAfterAPIRetryGaveUp(ctx, qtx, task, errMsg)
		return err
	}); err != nil {
		return nil, err
	}
	return payload, nil
}

// restoreChatDraftAfterAPIRetryGaveUp gives a chat member their prompt back
// when the daemon failed the turn because the model API kept failing before
// the turn produced any output (taskfailure.IsClaudeAPIRetryGaveUp).
//
// Sending cleared the composer, and a plain failure keeps the prompt only as a
// sent message, so without this the member has to retype it. The restore is
// the one a cancelled empty turn gets: the triggering input is deleted, a
// durable chat_draft_restore row is written in the caller's transaction, and
// the returned payload is broadcast as chat:cancel_finalized "restored" once
// it commits, which makes the initiator's composer fetch and apply it. The
// failure itself is still written as an assistant message by the failure path,
// so the reason stays visible.
//
// It applies under the same conditions as the cancel restore: the transcript
// is empty (model API retry rows do not count, see HasTaskMessages), and the
// input is the member's own — not a channel-ingested batch or a wakeup turn.
// The caller calls this only for a terminal, non-retried chat failure.
//
// Run it through restoreGaveUpChatDraft, never inside the terminal write's
// transaction: a restore that fails must not take the failure state with it.
func restoreChatDraftAfterAPIRetryGaveUp(ctx context.Context, qtx *db.Queries, task db.AgentTaskQueue, errMsg string) (*protocol.ChatCancelFinalizedPayload, error) {
	if !task.ChatSessionID.Valid || !taskfailure.IsClaudeAPIRetryGaveUp(errMsg) {
		return nil, nil
	}
	hasMessages, err := qtx.HasTaskMessages(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("list api-retry failed chat task messages: %w", err)
	}
	if hasMessages {
		return nil, nil
	}
	inputOwnerID := chatInputOwnerID(task)
	channelIngested, err := qtx.TaskHasChannelIngestedMessages(ctx, inputOwnerID)
	if err != nil {
		return nil, fmt.Errorf("check api-retry failed chat channel provenance: %w", err)
	}
	_, wakeupTurn, err := chatWakeupTurnInput(ctx, qtx, task)
	if err != nil {
		return nil, err
	}
	if channelIngested || wakeupTurn {
		return nil, nil
	}

	// Detach attachments before deleting the message: the attachment FK
	// cascades on delete, and the restored draft re-binds them.
	detached, err := qtx.DetachAttachmentsFromUserChatMessageByTask(ctx, inputOwnerID)
	if err != nil {
		return nil, fmt.Errorf("detach api-retry failed chat attachments: %w", err)
	}
	deleted, err := deleteUserChatInput(ctx, qtx, inputOwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete api-retry failed chat input: %w", err)
	}
	attachmentIDs := make([]pgtype.UUID, 0, len(detached))
	for _, a := range detached {
		attachmentIDs = append(attachmentIDs, a.ID)
	}
	if _, err := qtx.CreateChatDraftRestore(ctx, db.CreateChatDraftRestoreParams{
		ID:            deleted.ID,
		ChatSessionID: task.ChatSessionID,
		TaskID:        task.ID,
		Content:       deleted.Content,
		AttachmentIds: attachmentIDs,
	}); err != nil {
		return nil, fmt.Errorf("create api-retry failed chat draft restore: %w", err)
	}
	return &protocol.ChatCancelFinalizedPayload{
		Outcome:         protocol.ChatCancelOutcomeRestored,
		ChatSessionID:   util.UUIDToString(task.ChatSessionID),
		TaskID:          util.UUIDToString(task.ID),
		InitiatorUserID: util.UUIDToString(task.InitiatorUserID),
		MessageID:       util.UUIDToString(deleted.ID),
	}, nil
}
