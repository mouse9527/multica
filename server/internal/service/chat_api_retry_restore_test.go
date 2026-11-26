// This file belongs to the mouse9527/multica fork (not present upstream).
// M-312 / mouse9527/multica#209: a chat turn that only ever retried the model
// API keeps its prompt, whether it fails fast or the member cancels it.

package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// apiRetryGaveUpError is the shape of the daemon's fail-fast error
// (pkg/agent/claude_api_retry.go), including the stderr tail it may append.
const apiRetryGaveUpError = taskfailure.ClaudeAPIRetryGaveUpPrefix +
	" with HTTP 504 (server_error) and the turn produced no output; gave up after 4 retries (Claude Code allows 10). Set MULTICA_CLAUDE_API_RETRY_FAIL_AFTER on the daemon to change this (0 disables).; claude stderr: (none)"

// insertAPIRetryRows writes the transcript rows the daemon persists for model
// API retries: type "error", tool "api_retry".
func (f cancelFinalizeFixture) insertAPIRetryRows(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for seq := 1; seq <= n; seq++ {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO task_message (task_id, seq, type, tool, content)
			VALUES ($1, $2, 'error', $3, 'Model API returned HTTP 504 (server_error); Claude Code is retrying')
		`, f.taskID, seq, protocol.TaskMessageToolAPIRetry); err != nil {
			t.Fatalf("insert api_retry task message: %v", err)
		}
	}
}

func (f cancelFinalizeFixture) insertTranscriptRowAt(t *testing.T, ctx context.Context, seq int) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO task_message (task_id, seq, type, content)
		VALUES ($1, $2, 'text', 'partial output')
	`, f.taskID, seq); err != nil {
		t.Fatalf("insert task message: %v", err)
	}
}

func (f cancelFinalizeFixture) failureReason(t *testing.T, ctx context.Context) string {
	t.Helper()
	var status, reason string
	if err := f.pool.QueryRow(ctx, `
		SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id = $1
	`, f.taskID).Scan(&status, &reason); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "failed" {
		t.Fatalf("task status = %q, want failed", status)
	}
	return reason
}

// The daemon gave up after retries whose progress rows are already in the
// transcript: the turn still had no output, so the prompt (with its
// attachment) goes back to the composer, the failure stays visible as an
// assistant message, and the initiator's client is told to fetch the restore.
func TestFailTask_APIRetryGaveUp_RestoresDraftDespiteRetryRows(t *testing.T) {
	ctx := context.Background()
	pool := newCancelFinalizePool(t)
	f := createCancelFinalizeFixture(t, ctx, pool, "running", true)
	attachmentID := f.attachUserMessageAttachment(t, ctx)
	f.insertAPIRetryRows(t, ctx, 4)
	bus := events.New()
	rec := recordCancelFinalizedEvents(bus)
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	if _, _, err := svc.FailTaskWithTransition(ctx, util.MustParseUUID(f.taskID), protocol.TaskFailRequest{
		Error: apiRetryGaveUpError,
	}); err != nil {
		t.Fatalf("fail task: %v", err)
	}

	if got := f.failureReason(t, ctx); got != taskfailure.ReasonAgentProviderServerError.String() {
		t.Errorf("failure_reason = %q, want %q (not auto-retried)", got, taskfailure.ReasonAgentProviderServerError)
	}
	if f.userMessageExists(t, ctx) {
		t.Error("the triggering user message should have been moved back to the composer")
	}
	restores := f.draftRestores(t, ctx)
	if len(restores) != 1 {
		t.Fatalf("expected 1 chat_draft_restore row, got %d", len(restores))
	}
	if got := util.UUIDToString(restores[0].ID); got != f.userMessageID {
		t.Errorf("restore id = %q, want deleted message id %q", got, f.userMessageID)
	}
	if restores[0].Content != "run the thing" {
		t.Errorf("restore content = %q, want %q", restores[0].Content, "run the thing")
	}
	if len(restores[0].AttachmentIds) != 1 || util.UUIDToString(restores[0].AttachmentIds[0]) != attachmentID {
		t.Errorf("restore attachment ids = %v, want [%s]", restores[0].AttachmentIds, attachmentID)
	}
	if got := f.assistantMessages(t, ctx); len(got) != 1 || got[0] != apiRetryGaveUpError {
		t.Errorf("assistant messages = %v, want the failure text", got)
	}

	evts := rec.snapshot()
	if len(evts) != 1 {
		t.Fatalf("expected 1 chat:cancel_finalized event, got %d", len(evts))
	}
	payload, ok := evts[0].Payload.(protocol.ChatCancelFinalizedPayload)
	if !ok {
		t.Fatalf("payload type = %T", evts[0].Payload)
	}
	if payload.Outcome != protocol.ChatCancelOutcomeRestored || payload.MessageID != f.userMessageID || payload.TaskID != f.taskID {
		t.Errorf("payload = %+v, want restored outcome for the deleted message", payload)
	}
	if payload.Content != "" {
		t.Errorf("restored event must not carry the prompt content, got %q", payload.Content)
	}
	if evts[0].ChatSessionOwnerID != f.creatorID {
		t.Errorf("owner = %q, want session creator %q", evts[0].ChatSessionOwnerID, f.creatorID)
	}
}

// Output before the give-up keeps the prompt where it is: the turn did
// something, exactly as a cancelled non-empty turn keeps its prompt.
func TestFailTask_APIRetryGaveUp_KeepsPromptAfterOutput(t *testing.T) {
	ctx := context.Background()
	pool := newCancelFinalizePool(t)
	f := createCancelFinalizeFixture(t, ctx, pool, "running", true)
	f.insertAPIRetryRows(t, ctx, 2)
	f.insertTranscriptRowAt(t, ctx, 3)
	bus := events.New()
	rec := recordCancelFinalizedEvents(bus)
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	if _, _, err := svc.FailTaskWithTransition(ctx, util.MustParseUUID(f.taskID), protocol.TaskFailRequest{
		Error: apiRetryGaveUpError,
	}); err != nil {
		t.Fatalf("fail task: %v", err)
	}

	if !f.userMessageExists(t, ctx) {
		t.Error("user chat message should be preserved")
	}
	if got := f.draftRestores(t, ctx); len(got) != 0 {
		t.Errorf("expected no draft restore, got %d", len(got))
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("expected no chat:cancel_finalized event, got %d", got)
	}
}

// Any other failure is unchanged: the prompt stays a sent message, even with
// an empty transcript.
func TestFailTask_OtherFailure_KeepsPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newCancelFinalizePool(t)
	f := createCancelFinalizeFixture(t, ctx, pool, "running", true)
	bus := events.New()
	rec := recordCancelFinalizedEvents(bus)
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	const errText = "API Error: 504 Gateway Time-out"
	if _, _, err := svc.FailTaskWithTransition(ctx, util.MustParseUUID(f.taskID), protocol.TaskFailRequest{
		Error: errText,
	}); err != nil {
		t.Fatalf("fail task: %v", err)
	}

	if !f.userMessageExists(t, ctx) {
		t.Error("user chat message should be preserved")
	}
	if got := f.draftRestores(t, ctx); len(got) != 0 {
		t.Errorf("expected no draft restore, got %d", len(got))
	}
	if got := f.assistantMessages(t, ctx); len(got) != 1 || got[0] != errText {
		t.Errorf("assistant messages = %v, want [%q]", got, errText)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("expected no chat:cancel_finalized event, got %d", got)
	}
}

// Retry progress rows do not make a cancelled turn "non-empty": the member who
// stops a turn that only retried gets the prompt back, as before the rows
// existed.
func TestCancelTask_OnlyAPIRetryRows_StillRestoresDraft(t *testing.T) {
	ctx := context.Background()
	pool := newCancelFinalizePool(t)
	f := createCancelFinalizeFixture(t, ctx, pool, "running", true)
	f.insertAPIRetryRows(t, ctx, 3)
	bus := events.New()
	rec := recordCancelFinalizedEvents(bus)
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	result, err := svc.CancelTaskWithResult(ctx, util.MustParseUUID(f.taskID), CancelTaskOptions{ClientSupportsDraftRestore: true})
	if err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if result.CancelledChatMessage != nil {
		t.Fatalf("a started empty turn defers its judgment, got %+v", result.CancelledChatMessage)
	}
	if got := f.assistantMessages(t, ctx); len(got) != 0 {
		t.Fatalf("retry rows must not settle the turn as Stopped., got %v", got)
	}
	if got := f.chatFinalizeDeferredAt(t, ctx); got == nil {
		t.Fatal("chat_finalize_deferred_at should be set: the transcript is still empty")
	}

	svc.FinalizeDeferredCancelledChat(ctx, util.MustParseUUID(f.taskID))

	if f.userMessageExists(t, ctx) {
		t.Error("user chat message should have been deleted")
	}
	if restores := f.draftRestores(t, ctx); len(restores) != 1 || restores[0].Content != "run the thing" {
		t.Fatalf("draft restores = %+v, want the prompt", restores)
	}
	evts := rec.snapshot()
	if len(evts) != 1 {
		t.Fatalf("expected 1 chat:cancel_finalized event, got %d", len(evts))
	}
	if payload, _ := evts[0].Payload.(protocol.ChatCancelFinalizedPayload); payload.Outcome != protocol.ChatCancelOutcomeRestored {
		t.Errorf("outcome = %q, want restored", payload.Outcome)
	}
}
