// This file belongs to the mouse9527/multica fork (not present upstream).

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func apiRetryFrame(attempt int, status string, extra string) string {
	return `{"type":"system","subtype":"api_retry","attempt":` + strconv.Itoa(attempt) +
		`,"max_retries":10,"retry_delay_ms":2140,"error_status":` + status +
		`,"error":"server_error","session_id":"sess-retry"` + extra + `}`
}

func TestParseClaudeAPIRetry(t *testing.T) {
	t.Parallel()

	var msg claudeSDKMessage
	if err := json.Unmarshal([]byte(apiRetryFrame(3, "504", "")), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok := parseClaudeAPIRetry(msg)
	if !ok {
		t.Fatal("api_retry frame was not recognised")
	}
	if ev.attempt != 3 || ev.maxRetries != 10 || ev.errorStatus != 504 || ev.errorKind != "server_error" || ev.subagent {
		t.Fatalf("unexpected parse: %+v", ev)
	}
	if ev.retryDelay != 2140*time.Millisecond {
		t.Fatalf("retry delay = %s, want 2.14s", ev.retryDelay)
	}
	if got := ev.progressText(); !strings.Contains(got, "HTTP 504 (server_error)") || !strings.Contains(got, "attempt 3/10") || !strings.Contains(got, "2.1s") {
		t.Fatalf("progress text %q lacks status, attempt or delay", got)
	}

	// A request that failed without an HTTP response reports error_status null.
	var noStatus claudeSDKMessage
	if err := json.Unmarshal([]byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":null,"error":"unknown"}`), &noStatus); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok = parseClaudeAPIRetry(noStatus)
	if !ok || ev.errorStatus != 0 || ev.describeError() != "unknown" {
		t.Fatalf("null error_status parse: ok=%v %+v", ok, ev)
	}

	for _, frame := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"system","session_id":"s"}`,
		`{"type":"assistant","message":{"content":[]}}`,
	} {
		var other claudeSDKMessage
		if err := json.Unmarshal([]byte(frame), &other); err != nil {
			t.Fatalf("unmarshal %s: %v", frame, err)
		}
		if _, ok := parseClaudeAPIRetry(other); ok {
			t.Fatalf("%s must not parse as api_retry", frame)
		}
	}
}

func TestClaudeAPIRetryFailureTextClassifiesAsProviderError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		kind   string
		want   taskfailure.Reason
	}{
		{504, "server_error", taskfailure.ReasonAgentProviderServerError},
		{502, "", taskfailure.ReasonAgentProviderServerError},
		{529, "overloaded", taskfailure.ReasonAgentProviderCapacityOrRateLimit},
		{429, "rate_limit", taskfailure.ReasonAgentProviderCapacityOrRateLimit},
	}
	for _, tc := range cases {
		ev := claudeAPIRetryEvent{attempt: 4, maxRetries: 10, errorStatus: tc.status, errorKind: tc.kind}
		text := ev.failureText(4)
		if got := taskfailure.Classify(text); got != tc.want {
			t.Errorf("failure text %q classifies as %q, want %q", text, got, tc.want)
		}
	}
}

type apiRetryRun struct {
	result   Result
	messages []Message
	elapsed  time.Duration
}

// runClaudeAPIRetryFixture runs the claude backend against a fake executable
// that replays frames on stdout and then, when hang is set, keeps running the
// way the CLI does while it waits for its next retry.
func runClaudeAPIRetryFixture(t *testing.T, frames []string, failAfter int, hang bool) apiRetryRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	fixturePath := filepath.Join(t.TempDir(), "frames.jsonl")
	if err := os.WriteFile(fixturePath, []byte(strings.Join(frames, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	script := "#!/bin/sh\nIFS= read -r _\ncat " + fixturePath + "\n"
	if hang {
		script += "sleep 30\n"
	}
	fakePath := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := New("claude", Config{
		ExecutablePath:          fakePath,
		Env:                     map[string]string{"IS_SANDBOX": "1"},
		Logger:                  slog.Default(),
		ClaudeAPIRetryFailAfter: failAfter,
	})
	if err != nil {
		t.Fatalf("new claude backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var (
		mu       sync.Mutex
		messages []Message
		drained  = make(chan struct{})
	)
	go func() {
		defer close(drained)
		for m := range session.Messages {
			mu.Lock()
			messages = append(messages, m)
			mu.Unlock()
		}
	}()

	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a value")
		}
		<-drained
		mu.Lock()
		defer mu.Unlock()
		return apiRetryRun{result: result, messages: messages, elapsed: time.Since(start)}
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for result")
		return apiRetryRun{}
	}
}

func apiRetryStatuses(messages []Message) []Message {
	var out []Message
	for _, m := range messages {
		if m.Type == MessageStatus && m.Status == MessageStatusAPIRetry {
			out = append(out, m)
		}
	}
	return out
}

const (
	apiRetryInitFrame      = `{"type":"system","subtype":"init","session_id":"sess-retry"}`
	apiRetryAssistantFrame = `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Working on it."}]},"session_id":"sess-retry"}`
	apiRetryResultFrame    = `{"type":"result","subtype":"success","is_error":false,"session_id":"sess-retry","result":"Done."}`
)

// Each api_retry reaches the message stream as a progress status carrying the
// attempt and the API error, and a run that recovers still completes.
func TestClaudeExecuteReportsAPIRetryAsProgress(t *testing.T) {
	t.Parallel()

	run := runClaudeAPIRetryFixture(t, []string{
		apiRetryInitFrame,
		apiRetryFrame(1, "504", ""),
		apiRetryFrame(2, "504", ""),
		apiRetryAssistantFrame,
		apiRetryResultFrame,
	}, 4, false)

	if run.result.Status != "completed" || run.result.Output != "Done." {
		t.Fatalf("result = %+v, want completed with the final answer", run.result)
	}
	retries := apiRetryStatuses(run.messages)
	if len(retries) != 2 {
		t.Fatalf("got %d api_retry statuses, want 2: %+v", len(retries), run.messages)
	}
	for i, m := range retries {
		if !strings.Contains(m.Content, "HTTP 504") || !strings.Contains(m.Content, "attempt "+strconv.Itoa(i+1)+"/10") {
			t.Fatalf("retry status %d content = %q", i+1, m.Content)
		}
		if m.SessionID != "sess-retry" {
			t.Fatalf("retry status %d lost the session id: %+v", i+1, m)
		}
	}
}

// Reaching the threshold with no output ends the run now, with a failure that
// names the API error — not after the CLI's remaining retries.
func TestClaudeExecuteFailsFastOnRepeatedAPIRetriesWithoutOutput(t *testing.T) {
	t.Parallel()

	run := runClaudeAPIRetryFixture(t, []string{
		apiRetryInitFrame,
		apiRetryFrame(1, "504", ""),
		apiRetryFrame(2, "504", ""),
		apiRetryFrame(3, "504", ""),
		apiRetryFrame(4, "504", ""),
	}, 4, true)

	if run.result.Status != "failed" {
		t.Fatalf("status = %q (error %q), want failed", run.result.Status, run.result.Error)
	}
	if !strings.Contains(run.result.Error, "HTTP 504") || !strings.Contains(run.result.Error, "no output") {
		t.Fatalf("error %q does not explain the failure", run.result.Error)
	}
	if reason := taskfailure.Classify(run.result.Error); reason != taskfailure.ReasonAgentProviderServerError {
		t.Fatalf("error %q classifies as %q, want %q", run.result.Error, reason, taskfailure.ReasonAgentProviderServerError)
	}
	if run.elapsed >= 15*time.Second {
		t.Fatalf("run took %s; it should stop instead of waiting for the CLI", run.elapsed)
	}
	if run.result.Output != "" {
		t.Fatalf("a failed run must not carry output, got %q", run.result.Output)
	}
}

// Retries after the turn has shown the user something never fail it.
func TestClaudeExecuteKeepsRetriesAfterOutput(t *testing.T) {
	t.Parallel()

	frames := []string{apiRetryInitFrame, apiRetryAssistantFrame}
	for i := 1; i <= 6; i++ {
		frames = append(frames, apiRetryFrame(i, "504", ""))
	}
	frames = append(frames, apiRetryResultFrame)
	run := runClaudeAPIRetryFixture(t, frames, 4, false)

	if run.result.Status != "completed" || run.result.Output != "Done." {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	if got := len(apiRetryStatuses(run.messages)); got != 6 {
		t.Fatalf("got %d api_retry statuses, want 6", got)
	}
}

// Streamed text counts as output too, even before the assistant snapshot, and
// it stays counted when the retried request starts a new message.
func TestClaudeExecuteKeepsRetriesAfterStreamedText(t *testing.T) {
	t.Parallel()

	frames := []string{
		apiRetryInitFrame,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_a"}},"session_id":"sess-retry"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Partial"}},"session_id":"sess-retry"}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_b"}},"session_id":"sess-retry"}`,
	}
	for i := 1; i <= 5; i++ {
		frames = append(frames, apiRetryFrame(i, "504", ""))
	}
	frames = append(frames, apiRetryResultFrame)
	run := runClaudeAPIRetryFixture(t, frames, 4, false)

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
}

// A threshold of 0 leaves retrying to the CLI, and a subagent's retries never
// decide the main turn.
func TestClaudeExecuteAPIRetryFailFastDisabledAndSubagent(t *testing.T) {
	t.Parallel()

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		frames := []string{apiRetryInitFrame}
		for i := 1; i <= 8; i++ {
			frames = append(frames, apiRetryFrame(i, "504", ""))
		}
		frames = append(frames, apiRetryResultFrame)
		run := runClaudeAPIRetryFixture(t, frames, 0, false)
		if run.result.Status != "completed" {
			t.Fatalf("result = %+v, want completed", run.result)
		}
		if got := len(apiRetryStatuses(run.messages)); got != 8 {
			t.Fatalf("got %d api_retry statuses, want 8 (progress is reported even when fail-fast is off)", got)
		}
	})

	t.Run("subagent", func(t *testing.T) {
		t.Parallel()
		frames := []string{apiRetryInitFrame}
		for i := 1; i <= 5; i++ {
			frames = append(frames, apiRetryFrame(i, "504", `,"parent_tool_use_id":"toolu_sub"`))
		}
		frames = append(frames, apiRetryResultFrame)
		run := runClaudeAPIRetryFixture(t, frames, 4, false)
		if run.result.Status != "completed" {
			t.Fatalf("result = %+v, want completed", run.result)
		}
	})
}
