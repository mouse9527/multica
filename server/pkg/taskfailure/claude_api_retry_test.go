// This file belongs to the mouse9527/multica fork (not present upstream).

package taskfailure

import "testing"

func TestIsClaudeAPIRetryGaveUp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		errMsg string
		want   bool
	}{
		{ClaudeAPIRetryGaveUpPrefix + " with HTTP 504 (server_error) and the turn produced no output; gave up after 4 retries", true},
		{"  " + ClaudeAPIRetryGaveUpPrefix + " with HTTP 529; claude stderr: overloaded", true},
		{"API Error: 504 Gateway Time-out", false},
		{"execution cancelled; " + ClaudeAPIRetryGaveUpPrefix, false},
		{"", false},
	} {
		if got := IsClaudeAPIRetryGaveUp(tc.errMsg); got != tc.want {
			t.Errorf("IsClaudeAPIRetryGaveUp(%q) = %v, want %v", tc.errMsg, got, tc.want)
		}
	}
}
