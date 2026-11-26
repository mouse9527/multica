// This file belongs to the mouse9527/multica fork (not present upstream).

package daemon

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestClaudeAPIRetryFailAfterFromEnv(t *testing.T) {
	cases := []struct {
		value   string
		want    int
		wantErr bool
	}{
		{"", DefaultClaudeAPIRetryFailAfter, false},
		{"0", 0, false},
		{"6", 6, false},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("MULTICA_CLAUDE_API_RETRY_FAIL_AFTER", tc.value)
			got, err := claudeAPIRetryFailAfterFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for %q, got %d", tc.value, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%d, %v), want %d", got, err, tc.want)
			}
		})
	}
}

func TestAPIRetryTranscriptContent(t *testing.T) {
	t.Parallel()

	retry := agent.Message{Type: agent.MessageStatus, Status: agent.MessageStatusAPIRetry, Content: "Model API returned HTTP 504; Claude Code is retrying (attempt 3/10)", SessionID: "s"}
	got, ok := apiRetryTranscriptContent(retry)
	if !ok || got != retry.Content {
		t.Fatalf("api_retry status: got (%q, %v)", got, ok)
	}

	for _, msg := range []agent.Message{
		{Type: agent.MessageStatus, Status: "running", SessionID: "s"},
		{Type: agent.MessageStatus, Status: agent.MessageStatusAPIRetry},
		{Type: agent.MessageError, Content: "boom"},
		{Type: agent.MessageText, Content: "hi"},
	} {
		if _, ok := apiRetryTranscriptContent(msg); ok {
			t.Fatalf("%+v must not become an api_retry transcript row", msg)
		}
	}

	// The retry row is progress, not output: a resumed session that only ever
	// retried must still count as silent when it is cancelled (MUL-8188).
	if isTaskOutputReceived(retry) {
		t.Fatal("an api_retry status must not count as task output")
	}
}
