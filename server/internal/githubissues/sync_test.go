package githubissues

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newManualSyncEnv is the poll environment with the switches at their
// defaults: no poll, no refresh on view.
func newManualSyncEnv(t *testing.T) *pollEnv {
	t.Helper()
	return newPollEnvWith(t, nil)
}

func (env *pollEnv) sync(t *testing.T, issue db.Issue) SyncResult {
	t.Helper()
	result, err := env.svc.SyncIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return result
}

// quotaHeaders makes every answer of the fake report remaining requests in a
// window that resets at reset.
func (env *pollEnv) quotaHeaders(remaining int, reset time.Time) {
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	env.api.extra = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		return false
	}
}

// With the switches at their defaults nothing reads GitHub on its own: no
// poll job is registered and opening an issue sends nothing.
func TestSwitchesOffSendNothing(t *testing.T) {
	cfg := LoadConfig(envOf(map[string]string{EnvEnabled: "true", EnvToken: fakeToken, EnvTokenExpiresAt: "never"}))
	if cfg.PollEnabled() || cfg.RefreshOnView() {
		t.Fatalf("defaults: poll %v, refresh on view %v; want both off", cfg.PollEnabled(), cfg.RefreshOnView())
	}

	svc := newTestService(t, "http://127.0.0.1:0", "never")
	// A nil pool would fail the scheduler: StartPolling must not get that far.
	if err := svc.StartPolling(context.Background(), nil); err != nil {
		t.Fatalf("StartPolling with the poll off: %v", err)
	}
	if !strings.Contains(svc.logs.String(), "github issues poll off") {
		t.Fatalf("logs = %s, want the poll-off line", svc.logs.String())
	}

	env := newManualSyncEnv(t)
	env.api.putIssue(1, "stranger", "Crash on start", "It crashes.", "open", testNow.Add(-time.Hour), nil)
	issue := env.loadIssue(t, env.importIssue(t, 1).id)
	before := len(env.api.seen())
	env.api.putComment(501, 1, "stranger", "Same here", testNow.Add(-time.Minute))
	for range 3 {
		env.view(issue)
	}
	if after := len(env.api.seen()); after != before {
		t.Fatalf("opening the issue sent %d requests with the refresh off", after-before)
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issue.ID); n != 0 {
		t.Fatalf("comments stored without a sync: %d", n)
	}
}

// TestManualSyncPicksUpGitHubChanges is the acceptance path: what changed on
// GitHub (an edit, the issue closed, a new comment, an edited comment) is in
// Multica after one sync, in a few requests; syncing again stores nothing
// twice, and is not held back by the on-view refresh interval.
func TestManualSyncPicksUpGitHubChanges(t *testing.T) {
	env := newManualSyncEnv(t)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash on start", "It crashes.", "open", t0, nil)
	env.api.putComment(501, 1, "stranger", "Same here", t0.Add(time.Minute))
	issue := env.loadIssue(t, env.importIssue(t, 1).id)

	env.api.putIssue(1, "stranger", "Crash on start (macOS)", "It crashes.", "closed", testNow.Add(-time.Minute), nil)
	env.api.putComment(501, 1, "stranger", "Same here, on 14.2", testNow.Add(-50*time.Second))
	env.api.putComment(502, 1, "stranger", "Me too", testNow.Add(-40*time.Second))
	before := len(env.api.seen())

	got := env.sync(t, issue)
	want := SyncResult{IssueID: util.UUIDToString(issue.ID), IssueUpdated: true, NewComments: 1, CommentsUpdated: 1, RequestsUsed: 2}
	if got != want {
		t.Fatalf("sync = %+v, want %+v", got, want)
	}
	if sent := len(env.api.seen()) - before; sent != got.RequestsUsed {
		t.Fatalf("requests_used %d, GitHub saw %d", got.RequestsUsed, sent)
	}
	stored := env.issue(t, 1)
	if stored.title != "Crash on start (macOS)" || stored.state != "closed" {
		t.Fatalf("issue after sync = %+v", stored)
	}
	var texts []string
	rows, err := env.svc.db.Query(context.Background(), `SELECT content FROM comment WHERE issue_id = $1 ORDER BY created_at, id`, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		texts = append(texts, s)
	}
	rows.Close()
	if len(texts) != 2 || !strings.Contains(texts[0], "on 14.2") || !strings.Contains(texts[1], "Me too") {
		t.Fatalf("comments after sync = %q", texts)
	}

	// Again at once: GitHub is read again, nothing is stored twice.
	again := env.sync(t, issue)
	if again.IssueUpdated || again.NewComments != 0 || again.CommentsUpdated != 0 || again.RequestsUsed != 2 {
		t.Fatalf("second sync = %+v", again)
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issue.ID); n != 2 {
		t.Fatalf("comments after two syncs = %d", n)
	}
}

// A sync reads the linked pull requests too, with the poller's rule: only
// those that close a tracked issue.
func TestManualSyncMirrorsLinkedPulls(t *testing.T) {
	env := newManualSyncEnv(t)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash on start", "", "open", t0, nil)
	issue := env.loadIssue(t, env.importIssue(t, 1).id)
	env.api.putPull(7, "Fix crash", "Closes #1", "open", t0)
	env.api.putPull(8, "Refactor", "see #1", "open", t0)
	env.api.mention(1, 7)
	env.api.mention(1, 8)
	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)

	got := env.sync(t, issue)
	if got.PullsUpdated != 1 || got.RequestsUsed != 4 {
		t.Fatalf("sync = %+v, want 1 pull request in 4 requests (issue, comments, timeline, pull)", got)
	}
	if numbers := mirror.numbers(); len(numbers) != 1 || numbers[0] != 7 {
		t.Fatalf("mirrored %v, want [7]", numbers)
	}
	if again := env.sync(t, issue); again.PullsUpdated != 0 || again.RequestsUsed != 3 {
		t.Fatalf("second sync = %+v, want no pull request read", again)
	}
}

// The quota guard: below the threshold a sync is refused before any request,
// with the wait until GitHub's window resets; once it has reset, syncs run
// again. A rate-limit pause GitHub asked for refuses it the same way.
func TestManualSyncQuotaGuard(t *testing.T) {
	env := newManualSyncEnv(t)
	reset := testNow.Add(30 * time.Minute)
	env.quotaHeaders(4000, reset)
	env.api.putIssue(1, "stranger", "Crash on start", "", "open", testNow.Add(-time.Hour), nil)
	issue := env.loadIssue(t, env.importIssue(t, 1).id)

	got := env.sync(t, issue)
	if got.RateLimit == nil || got.RateLimit.Remaining != 4000 || !got.RateLimit.ResetAt.Equal(reset.Truncate(time.Second)) {
		t.Fatalf("rate limit = %+v", got.RateLimit)
	}

	env.quotaHeaders(defaultSyncMinRemaining-1, reset)
	env.sync(t, issue) // checked before its requests: still the old quota
	before := len(env.api.seen())
	_, err := env.svc.SyncIssue(context.Background(), issue)
	var low *QuotaLowError
	if !errors.As(err, &low) || low.Remaining != defaultSyncMinRemaining-1 || low.Threshold != defaultSyncMinRemaining ||
		low.RetryAfter != reset.Truncate(time.Second).Sub(testNow) {
		t.Fatalf("sync below the guard = %v (%+v)", err, low)
	}
	if sent := len(env.api.seen()) - before; sent != 0 {
		t.Fatalf("refused sync sent %d requests", sent)
	}

	*env.now = reset.Add(time.Second)
	if _, err := env.svc.SyncIssue(context.Background(), issue); err != nil {
		t.Fatalf("sync after the window reset: %v", err)
	}

	env.svc.pollGate.pause(env.now.Add(time.Minute))
	before = len(env.api.seen())
	_, err = env.svc.SyncIssue(context.Background(), issue)
	var limited *ghsnapshot.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != time.Minute || len(env.api.seen()) != before {
		t.Fatalf("sync during a pause = %v, sent %d", err, len(env.api.seen())-before)
	}
}

// A threshold set by the deployment replaces the default; a malformed one
// turns the feature off rather than guarding with a value nobody meant.
func TestSyncMinRemainingConfig(t *testing.T) {
	base := map[string]string{EnvEnabled: "true", EnvToken: fakeToken, EnvTokenExpiresAt: "never"}
	with := func(v string) Config {
		vars := map[string]string{EnvSyncMinRemaining: v}
		for k, val := range base {
			vars[k] = val
		}
		return LoadConfig(envOf(vars))
	}
	if cfg := with("250"); !cfg.Enabled() || cfg.syncMinRemaining != 250 {
		t.Fatalf("250: %+v", cfg)
	}
	for _, bad := range []string{"many", "-1"} {
		if cfg := with(bad); cfg.Enabled() || !strings.Contains(cfg.DisabledReason(), EnvSyncMinRemaining) {
			t.Fatalf("%q: enabled %v, reason %q", bad, cfg.Enabled(), cfg.DisabledReason())
		}
	}
	on := LoadConfig(envOf(map[string]string{EnvEnabled: "true", EnvToken: fakeToken, EnvTokenExpiresAt: "never",
		EnvPollEnabled: "true", EnvRefreshOnView: "true"}))
	if !on.PollEnabled() || !on.RefreshOnView() {
		t.Fatalf("switches set to true: poll %v, refresh %v", on.PollEnabled(), on.RefreshOnView())
	}
}

// A write is held back by nothing but logged while the quota is low.
func TestLowQuotaWriteIsSentAndLogged(t *testing.T) {
	svc := newTestService(t, "http://127.0.0.1:0", "never")
	svc.quota.observe(http.Header{"X-Ratelimit-Remaining": {"10"}, "X-Ratelimit-Reset": {strconv.FormatInt(testNow.Add(time.Hour).Unix(), 10)}})
	svc.warnLowQuota(http.MethodGet, "GET /repositories/1/issues/1")
	if strings.Contains(svc.logs.String(), "quota low") {
		t.Fatal("a read logged the low quota")
	}
	svc.warnLowQuota(http.MethodPost, "POST /repositories/1/issues/1/comments")
	if !strings.Contains(svc.logs.String(), "github quota low; write sent anyway") {
		t.Fatalf("logs = %s", svc.logs.String())
	}
}

// An issue deleted on GitHub, in a repository that still answers, is marked
// deleted and no longer synced; a 404 from a repository that no longer
// answers marks nothing.
func TestManualSyncDeletedIssue(t *testing.T) {
	env := newManualSyncEnv(t)
	env.api.putIssue(1, "stranger", "Crash on start", "", "open", testNow.Add(-time.Hour), nil)
	issue := env.loadIssue(t, env.importIssue(t, 1).id)
	repoPath := fmt.Sprintf("/repositories/%d", pollRepoID)
	repoStatus := http.StatusNotFound
	env.api.mu.Lock()
	env.api.issueReplies = map[int]issueReply{1: {status: http.StatusNotFound}}
	env.api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != repoPath {
			return false
		}
		w.WriteHeader(repoStatus)
		_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(pollRepoID) + `}`))
		return true
	}
	env.api.mu.Unlock()

	if _, err := env.svc.SyncIssue(context.Background(), issue); err == nil {
		t.Fatal("a 404 from a repository that answers 404 too was taken as a deletion")
	}
	if n := env.count(t, `SELECT count(*) FROM issue_external WHERE issue_id = $1 AND state_reason LIKE 'multica:%'`, issue.ID); n != 0 {
		t.Fatal("issue marked gone while the repository is unreachable")
	}

	env.api.mu.Lock()
	repoStatus = http.StatusOK
	env.api.mu.Unlock()
	if got := env.sync(t, issue); got.Gone != "deleted" || !got.IssueUpdated {
		t.Fatalf("sync of a deleted issue = %+v", got)
	}
	before := len(env.api.seen())
	if got := env.sync(t, issue); got.Gone != "deleted" || got.RequestsUsed != 0 || len(env.api.seen()) != before {
		t.Fatalf("sync of an issue already gone = %+v", got)
	}
}

// A local issue has nothing to sync.
func TestManualSyncRefusesLocalIssue(t *testing.T) {
	env := newManualSyncEnv(t)
	local := env.f.Issue(t, "Local only")
	issue := env.loadIssue(t, local)
	if _, err := env.svc.SyncIssue(context.Background(), issue); !errors.Is(err, ErrNotGitHubIssue) {
		t.Fatalf("sync of a local issue = %v", err)
	}
	if n := len(env.api.seen()); n != 0 {
		t.Fatalf("sync of a local issue sent %d requests", n)
	}
}

// Writes from Multica and manual syncs never make a second copy: the echo of
// a comment written from Multica is recognized, and a write whose local copy
// failed is healed by the sync of its issue, with its intent settled.
func TestManualSyncAfterWritesFromMultica(t *testing.T) {
	env := newWriteEnv(t)
	issue := env.importedIssue(t, 1)
	ctx := context.Background()

	w, err := env.svc.BeginCommentCreate(ctx, env.agent, issue, "Fixed in #2.")
	if err != nil {
		t.Fatal(err)
	}
	env.localComment(t, w, issue, env.agent, "Fixed in #2.")
	for range 2 {
		env.sync(t, issue)
	}
	if got := env.comments(t, issue); len(got) != 1 || got[0].authorType != "agent" {
		t.Fatalf("comments after write and syncs = %+v", got)
	}

	w2, err := env.svc.BeginCommentCreate(ctx, env.agent, issue, "Done")
	if err != nil {
		t.Fatal(err)
	}
	w2.Finish(ctx, errors.New("database unavailable"))
	if got := env.lastIntent(t); got.state != intentGitHubOK {
		t.Fatalf("intent = %+v", got)
	}
	*env.now = env.now.Add(pollInterval + time.Second)
	env.api.setNow(*env.now)
	for range 2 {
		env.sync(t, issue)
	}
	got := env.comments(t, issue)
	if len(got) != 2 || got[1].id != util.UUIDToString(w2.CommentID()) || got[1].content != "Done" {
		t.Fatalf("comments after heal = %+v", got)
	}
	if got := env.lastIntent(t); got.state != intentApplied {
		t.Fatalf("intent after sync = %+v", got)
	}
}
