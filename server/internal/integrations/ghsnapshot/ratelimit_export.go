// Added by the mouse9527/multica fork: exports the throttling helpers so
// server/internal/githubissues reuses GitHub's rate-limit detection and
// backoff instead of keeping a second copy.

package ghsnapshot

import (
	"net/http"
	"time"
)

// IsRateLimited reports whether a GitHub response is a primary or secondary
// rate limit (429, or a 403 carrying GitHub's throttling signals).
func IsRateLimited(resp *http.Response, body []byte) bool {
	return isRateLimited(resp, body)
}

// RateLimitFromResponse returns how long to wait before calling GitHub again,
// read from the throttling headers and clamped to [1s, 5m].
func RateLimitFromResponse(resp *http.Response, now time.Time) *RateLimitError {
	return rateLimitFromResponse(resp, now)
}
