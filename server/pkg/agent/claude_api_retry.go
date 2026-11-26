// This file belongs to the mouse9527/multica fork (not present upstream).
// M-312 / mouse9527/multica#209: surface Claude Code's api_retry events as
// task progress, and fail a turn early when the model API keeps failing before
// the turn has produced any output.

package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// claudeSubtypeAPIRetry is the system event Claude Code emits each time a
// model API request failed and the CLI is about to retry it on its own. The
// CLI retries silently otherwise: against a gateway that times out every
// request after ~2 minutes, a turn can sit for 20+ minutes with no output at
// all while the CLI works through its retries.
const claudeSubtypeAPIRetry = "api_retry"

// MessageStatusAPIRetry is the Message.Status of a MessageStatus that reports
// a model API retry. Its Content is a human-readable progress line; the daemon
// writes it to the task transcript so chat and the task page show it.
const MessageStatusAPIRetry = "api_retry"

// claudeAPIRetryFields are the api_retry fields of a stream-json system event:
//
//	{"type":"system","subtype":"api_retry","attempt":3,"max_retries":10,
//	 "retry_delay_ms":2140,"error_status":504,"error":"server_error", ...}
//
// They are embedded in claudeSDKMessage so the existing decoder reads them.
// error_status is null when the request failed without an HTTP response.
type claudeAPIRetryFields struct {
	Attempt      int             `json:"attempt,omitempty"`
	MaxRetries   int             `json:"max_retries,omitempty"`
	RetryDelayMs float64         `json:"retry_delay_ms,omitempty"`
	ErrorStatus  *int            `json:"error_status,omitempty"`
	APIError     json.RawMessage `json:"error,omitempty"`
}

// claudeAPIRetryState is a turn's api_retry bookkeeping, guarded by the
// turn's mu.
type claudeAPIRetryState struct {
	// sawStreamOutput sticks once streamed text or thinking reached the user.
	// The coalescer's own flags reset on every message_start, and a retried
	// request starts a new message.
	sawStreamOutput bool
	// failure is set when the turn gave up; it stays the turn's terminal
	// reason even if the stopping CLI still writes a result.
	failure string
}

// claudeAPIRetryEvent is one parsed api_retry event.
type claudeAPIRetryEvent struct {
	attempt     int
	maxRetries  int
	retryDelay  time.Duration
	errorStatus int // 0 when the CLI reported none
	errorKind   string
	subagent    bool
}

const claudeAPIRetryErrorKindMaxLen = 120

func parseClaudeAPIRetry(msg claudeSDKMessage) (claudeAPIRetryEvent, bool) {
	if msg.Type != "system" || msg.Subtype != claudeSubtypeAPIRetry {
		return claudeAPIRetryEvent{}, false
	}
	ev := claudeAPIRetryEvent{
		attempt:    msg.Attempt,
		maxRetries: msg.MaxRetries,
		retryDelay: time.Duration(msg.RetryDelayMs * float64(time.Millisecond)),
		errorKind:  claudeAPIRetryErrorKind(msg.APIError),
		subagent:   msg.ParentToolUseID != "",
	}
	if msg.ErrorStatus != nil {
		ev.errorStatus = *msg.ErrorStatus
	}
	return ev, true
}

// claudeAPIRetryErrorKind reads the event's error field. Claude Code sends a
// short category string ("server_error", "rate_limit", ...); an object with a
// message is accepted too so a future shape degrades to something readable.
func claudeAPIRetryErrorKind(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var obj struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return ""
		}
		s = obj.Message
		if s == "" {
			s = obj.Type
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > claudeAPIRetryErrorKindMaxLen {
		s = s[:claudeAPIRetryErrorKindMaxLen] + "…"
	}
	return s
}

// describeError renders the failure for people: "HTTP 504 (server_error)".
func (ev claudeAPIRetryEvent) describeError() string {
	switch {
	case ev.errorStatus != 0 && ev.errorKind != "":
		return fmt.Sprintf("HTTP %d (%s)", ev.errorStatus, ev.errorKind)
	case ev.errorStatus != 0:
		return fmt.Sprintf("HTTP %d", ev.errorStatus)
	case ev.errorKind != "":
		return ev.errorKind
	default:
		return "an error"
	}
}

func (ev claudeAPIRetryEvent) attemptLabel() string {
	if ev.maxRetries > 0 {
		return fmt.Sprintf("%d/%d", ev.attempt, ev.maxRetries)
	}
	return fmt.Sprintf("%d", ev.attempt)
}

// progressText is the transcript line shown while the CLI retries.
func (ev claudeAPIRetryEvent) progressText() string {
	who := "Model API"
	if ev.subagent {
		who = "Subagent model API"
	}
	text := fmt.Sprintf("%s returned %s; Claude Code is retrying (attempt %s", who, ev.describeError(), ev.attemptLabel())
	if ev.retryDelay > 0 {
		text += ", next try in " + ev.retryDelay.Round(100*time.Millisecond).String()
	}
	return text + ")"
}

// failureText is the task error when the daemon gives up before the CLI does.
// It starts with taskfailure.ClaudeAPIRetryGaveUpPrefix, which is how the
// server recognises the turn and gives a chat member their prompt back.
//
// It reads as the outcome of the turn, because it is shown as-is: the chat
// writes it as the assistant's message and the task page shows it in Run
// details. Operator knobs (the threshold is MULTICA_CLAUDE_API_RETRY_FAIL_AFTER,
// documented in the environment-variable docs) never belong in it.
//
// The wording is read by taskfailure.Classify: an HTTP status keeps the
// provider's own bucket (5xx → provider_server_error, 429/529 → capacity),
// which the platform does not auto-retry — re-running a turn the gateway just
// failed N times in a row would only repeat the wait. The text deliberately
// avoids "timeout"/"timed out", which would land in the retryable
// provider_network bucket.
func (ev claudeAPIRetryEvent) failureText(failAfter int) string {
	return fmt.Sprintf(
		"%s with %s and the turn produced no output; gave up after %d retries.",
		taskfailure.ClaudeAPIRetryGaveUpPrefix, ev.describeError(), failAfter,
	)
}

// noteAPIRetryLocked records an api_retry event on the turn and decides
// whether the turn should give up now. The caller holds t.mu.
//
// The turn gives up only when all of these hold:
//   - the daemon configured a threshold (ClaudeAPIRetryFailAfter > 0);
//   - this is the main thread's retry and has reached the threshold;
//   - the turn has produced no output yet: no assistant event, and no streamed
//     text or thinking. A turn that already showed the user something keeps
//     its retries, however many there are;
//   - nothing else has already decided the turn's outcome.
func (t *claudeTurn) noteAPIRetryLocked(ev claudeAPIRetryEvent) (failure string) {
	failAfter := t.backend.cfg.ClaudeAPIRetryFailAfter
	if failAfter <= 0 || ev.subagent || ev.attempt < failAfter {
		return ""
	}
	if t.claudeTurnProducedOutputLocked() || t.sawResult || t.terminalReasonError != "" || t.apiRetry.failure != "" {
		return ""
	}
	t.apiRetry.failure = ev.failureText(failAfter)
	t.terminalReasonError = t.apiRetry.failure
	return t.apiRetry.failure
}

// noteStreamOutputLocked records streamed text or thinking. The caller holds
// t.mu and has just fed a main-thread stream_event to the coalescer.
func (t *claudeTurn) noteStreamOutputLocked() {
	if t.coalescer.hasStreamedText() || t.coalescer.hasStreamedThinking() {
		t.apiRetry.sawStreamOutput = true
	}
}

// claudeTurnProducedOutputLocked reports whether this turn has shown the user
// anything. The caller holds t.mu.
func (t *claudeTurn) claudeTurnProducedOutputLocked() bool {
	return t.assistantEventCount > 0 || t.apiRetry.sawStreamOutput
}

// handleAPIRetry is the system-event hook for api_retry. It is called with
// t.mu held and releases it. It logs the retry, reports it as progress, and,
// when the turn gives up, stops the CLI so the run ends now with the failure
// recorded as the turn's terminal reason.
func (t *claudeTurn) handleAPIRetry(ev claudeAPIRetryEvent) {
	failure := t.noteAPIRetryLocked(ev)
	sid := t.sessionID
	t.mu.Unlock()

	logger := t.backend.cfg.Logger
	if logger != nil {
		attrs := []any{
			"task_id", t.backend.cfg.TaskID,
			"attempt", ev.attempt,
			"max_retries", ev.maxRetries,
			"error_status", ev.errorStatus,
			"error", ev.errorKind,
			"retry_delay", ev.retryDelay.Round(time.Millisecond).String(),
			"subagent", ev.subagent,
			"session_id", sid,
		}
		if failure != "" {
			logger.Warn("claude api_retry: no output after repeated model API failures; stopping the turn",
				append(attrs, "fail_after", t.backend.cfg.ClaudeAPIRetryFailAfter)...)
		} else {
			logger.Info("claude api_retry", attrs...)
		}
	}

	trySend(t.msgCh, Message{
		Type:      MessageStatus,
		Status:    MessageStatusAPIRetry,
		Content:   ev.progressText(),
		SessionID: sid,
	})

	if failure != "" {
		// terminate waits out the process group's grace window; it must not
		// run on the stdout reader goroutine that called us.
		go t.proc.terminate()
	}
}
