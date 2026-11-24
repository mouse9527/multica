// Added by the mouse9527/multica fork (M-237).

package cli

import (
	"strings"
	"testing"
)

// TestFormatErrorTaskScoped503IsRetryable pins the client half of M-237: the
// server answers 503 "auth backend unavailable" when it cannot look a task
// token up, and that must read as a temporary server problem the agent can
// retry — never as the "token rejected, do not retry" copy reserved for a
// task-scoped 401.
func TestFormatErrorTaskScoped503IsRetryable(t *testing.T) {
	httpErr := &HTTPError{
		Method:     "POST",
		Path:       "/api/issues/x/comments",
		StatusCode: 503,
		Body:       `{"error":"auth backend unavailable"}`,
		TaskScoped: true,
	}

	if got := httpErr.Kind(); got != KindServerError {
		t.Fatalf("Kind() = %v, want %v", got, KindServerError)
	}

	for _, tc := range []struct {
		lang    string
		want    []string
		refuted []string
	}{
		{"en_US.UTF-8", []string{"temporarily unavailable", "try again"}, []string{"rejected", "do not retry"}},
		{"zh_CN.UTF-8", []string{"暂时不可用", "重试"}, []string{"已被拒绝", "不要重试"}},
	} {
		withLang(t, tc.lang)
		got := FormatError(httpErr, false)
		for _, sub := range tc.want {
			if !strings.Contains(got, sub) {
				t.Errorf("%s: %q missing %q", tc.lang, got, sub)
			}
		}
		for _, sub := range tc.refuted {
			if strings.Contains(got, sub) {
				t.Errorf("%s: %q must not contain %q", tc.lang, got, sub)
			}
		}
	}

	if got := ExitCodeFor(httpErr); got == ExitAuth {
		t.Errorf("ExitCodeFor(task-scoped 503) = ExitAuth; a lookup outage is not an auth failure")
	}
}
