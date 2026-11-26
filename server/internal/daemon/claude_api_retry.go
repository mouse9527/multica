// This file belongs to the mouse9527/multica fork (not present upstream).
// M-312 / mouse9527/multica#209: Claude Code api_retry progress and the
// no-output fail-fast threshold.

package daemon

import (
	"fmt"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// DefaultClaudeAPIRetryFailAfter is how many consecutive model API retries a
// Claude Code turn may report before it has produced any output, after which
// the daemon stops the turn and fails the task with the API error.
//
// Claude Code retries a failed request up to 10 times on its own. Against a
// gateway that answers each request with a 504 after ~2 minutes, that is ~20
// minutes of silence per turn; a turn that has failed 4 times in a row has
// already been silent ~8 minutes and has rarely recovered afterwards. Short
// bursts — 429/529 or a quick 5xx — back off for seconds and usually clear
// within the first two or three retries, so 4 leaves those alone. A turn that
// already produced output is never cut short, however many retries it sees.
const DefaultClaudeAPIRetryFailAfter = 4

// claudeAPIRetryFailAfterFromEnv reads MULTICA_CLAUDE_API_RETRY_FAIL_AFTER.
// 0 disables the fail-fast and leaves retrying entirely to the CLI.
func claudeAPIRetryFailAfterFromEnv() (int, error) {
	const key = "MULTICA_CLAUDE_API_RETRY_FAIL_AFTER"
	n, err := intFromEnv(key, DefaultClaudeAPIRetryFailAfter)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: must be >= 0, got %d", key, n)
	}
	return n, nil
}

// apiRetryTranscriptContent returns the transcript line for a backend's model
// API retry status, and whether msg is one.
//
// The line is stored as an "error" row tagged with tool
// protocol.TaskMessageToolAPIRetry: chat and the task page already render
// error rows inline, so a retrying run shows "Model API returned HTTP 504 …;
// retrying (attempt 3/10 …)" instead of looking stuck. It is progress, not
// output, on both sides: the message is a MessageStatus, so a resumed session
// that only ever retried still counts as silent when it is cancelled; and the
// tag keeps the server's empty-transcript check (HasTaskMessages) false, so
// cancelling such a turn, or the turn giving up, still restores the draft.
func apiRetryTranscriptContent(msg agent.Message) (string, bool) {
	if msg.Type != agent.MessageStatus || msg.Status != agent.MessageStatusAPIRetry || msg.Content == "" {
		return "", false
	}
	return msg.Content, true
}
