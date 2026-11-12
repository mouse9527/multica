package githubissues

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// quotaState is the hourly request quota of the token as GitHub last
// reported it (X-RateLimit-Remaining and X-RateLimit-Reset on every
// answer). The quota is the token owner's, shared with everything else that
// uses the account (an agent's gh, for one), so it is read from GitHub rather
// than counted here.
type quotaState struct {
	mu        sync.Mutex
	known     bool
	limit     int
	remaining int
	resetAt   time.Time
}

// observe records the quota headers of one answer; an answer without them
// (a transport error page, a test server) leaves the last report.
func (q *quotaState) observe(h http.Header) {
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil || remaining < 0 {
		return
	}
	reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || reset <= 0 {
		return
	}
	limit, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	q.mu.Lock()
	defer q.mu.Unlock()
	q.known, q.limit, q.remaining, q.resetAt = true, limit, remaining, time.Unix(reset, 0).UTC()
}

// Quota is the request quota as GitHub last reported it.
type Quota struct {
	Limit     int       `json:"limit,omitempty"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

// current returns the last report, or false when GitHub has not reported one
// yet or its window has since reset, so the count it gave no longer holds.
func (q *quotaState) current(now time.Time) (Quota, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.known || !now.Before(q.resetAt) {
		return Quota{}, false
	}
	return Quota{Limit: q.limit, Remaining: q.remaining, ResetAt: q.resetAt}, true
}

// QuotaLowError refuses a manual sync because fewer requests than the
// threshold (MULTICA_GITHUB_SYNC_MIN_REMAINING) are left in GitHub's hourly
// quota. Nothing was sent. RetryAfter is when the quota resets.
type QuotaLowError struct {
	Remaining  int
	Threshold  int
	ResetAt    time.Time
	RetryAfter time.Duration
}

func (e *QuotaLowError) Error() string {
	return fmt.Sprintf("githubissues: only %d GitHub requests left this hour (guard %d); try again after %s",
		e.Remaining, e.Threshold, e.RetryAfter.Round(time.Second))
}

// checkSyncQuota is the quota guard of manual syncs: a rate-limit pause GitHub
// asked for is a *ghsnapshot.RateLimitError, and a quota below the threshold
// is a *QuotaLowError. Writes from Multica do not pass through it (see
// warnLowQuota).
func (s *Service) checkSyncQuota() error {
	now := s.now()
	if until := s.pollGate.until(); now.Before(until) {
		return &ghsnapshot.RateLimitError{RetryAfter: until.Sub(now)}
	}
	quota, ok := s.quota.current(now)
	if !ok || quota.Remaining >= s.cfg.syncMinRemaining {
		return nil
	}
	return &QuotaLowError{
		Remaining: quota.Remaining, Threshold: s.cfg.syncMinRemaining,
		ResetAt: quota.ResetAt, RetryAfter: quota.ResetAt.Sub(now),
	}
}

// warnLowQuota logs a write sent while the quota is below the manual sync
// guard. Writes are never held back by it: a write from Multica that waited
// for the quota would leave GitHub and Multica apart.
func (s *Service) warnLowQuota(method, op string) {
	if method == http.MethodGet {
		return
	}
	if quota, ok := s.quota.current(s.now()); ok && quota.Remaining < s.cfg.syncMinRemaining {
		s.logger.Warn("github quota low; write sent anyway", "op", op,
			"remaining", quota.Remaining, "guard", s.cfg.syncMinRemaining, "reset_at", quota.ResetAt)
	}
}

// QuotaStatus returns the request quota as GitHub last reported it, or nil
// when it has not reported one in the current window.
func (s *Service) QuotaStatus() *Quota {
	if s == nil {
		return nil
	}
	quota, ok := s.quota.current(s.now())
	if !ok {
		return nil
	}
	return &quota
}

type requestCounterKey struct{}

// withRequestCounter returns a context whose GitHub requests are counted in
// the returned counter, for the requests_used of a manual sync.
func withRequestCounter(ctx context.Context) (context.Context, *atomic.Int64) {
	n := new(atomic.Int64)
	return context.WithValue(ctx, requestCounterKey{}, n), n
}

func countRequest(ctx context.Context) {
	if n, ok := ctx.Value(requestCounterKey{}).(*atomic.Int64); ok {
		n.Add(1)
	}
}
