// This file belongs to the mouse9527/multica fork (not present upstream).
// M-312 / mouse9527/multica#209.

package taskfailure

import "strings"

// ClaudeAPIRetryGaveUpPrefix starts the error of a Claude Code turn the daemon
// stopped because the model API kept failing before the turn produced any
// output (pkg/agent/claude_api_retry.go). The server reads it to give a chat
// member their prompt back, the same way a cancelled empty turn does.
//
// The text is the daemon's own, never the CLI's, so matching its start is
// stable. It carries no "timeout" wording, which would classify as the
// retryable provider_network bucket.
const ClaudeAPIRetryGaveUpPrefix = "claude: model API kept failing"

// IsClaudeAPIRetryGaveUp reports whether errMsg is a no-output Claude turn the
// daemon gave up on after repeated model API retries.
func IsClaudeAPIRetryGaveUp(errMsg string) bool {
	return strings.HasPrefix(strings.TrimSpace(errMsg), ClaudeAPIRetryGaveUpPrefix)
}
