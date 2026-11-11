package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/scheduler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	pollJobName = "github_issues_poll"
	// pollInterval is the cycle of every registered repository.
	pollInterval = 60 * time.Second
	// pollTickInterval is how often the poll scheduler checks for a due
	// cycle, so a cycle starts at most this late.
	pollTickInterval = 10 * time.Second
	// pollRunTimeout bounds one cycle. Repositories left over when it runs
	// out are polled first in the next cycle (least recently polled first).
	pollRunTimeout = 50 * time.Second
	// pollWorkers is the number of GitHub requests in flight at once for the
	// one token. Each worker polls one repository at a time, so requests to
	// a repository are serial.
	pollWorkers = 2
	// pollOverlap is how far each cycle re-reads behind a cursor, so an item
	// whose updated_at GitHub reports late is not skipped.
	pollOverlap = 2 * time.Minute
	pollPerPage = 100
	// pollPagesPerStream bounds the requests of one stream in one cycle; a
	// stream with more to read continues in the next cycle where it stopped
	// (streamResume).
	pollPagesPerStream = 10
)

// StartPolling registers the poll job on a scheduler of its own and runs it
// until ctx is done. A cycle takes up to pollRunTimeout, and the scheduler runs
// handlers one after another, so sharing the main scheduler would hold back
// every other job (issue wakeups tick every 30 seconds) behind GitHub.
func (s *Service) StartPolling(ctx context.Context, pool *pgxpool.Pool) error {
	mgr := scheduler.NewManager(pool, scheduler.Options{TickInterval: pollTickInterval})
	if err := mgr.Register(s.PollJob()); err != nil {
		return err
	}
	go func() { _ = mgr.Run(ctx) }()
	return nil
}

// PollJob is the scheduler job that polls every registered repository once a
// minute. It is one global job rather than one per repository: the database
// lease then admits one cycle at a time across all replicas, which is what
// keeps the token at pollWorkers requests in flight.
func (s *Service) PollJob() scheduler.JobSpec {
	return scheduler.JobSpec{
		Name: pollJobName, Cadence: pollInterval, CatchUpMode: scheduler.CatchUpLatestOnly, CatchUpWindow: 10 * time.Minute,
		RunTimeout: pollRunTimeout, StaleTimeout: 2 * time.Minute, HeartbeatInterval: 15 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: scheduler.StaticScopes(scheduler.ScopeGlobal),
		Handler: func(ctx context.Context, _ scheduler.HandlerInput) (scheduler.HandlerResult, error) {
			stats, err := s.PollOnce(ctx)
			return scheduler.HandlerResult{RowsAffected: int64(stats.Synced), Result: stats.result()}, err
		},
	}
}

// PollStats summarizes one cycle.
type PollStats struct {
	Repos  int
	Synced int
	Failed int
	// RateLimited is true when GitHub throttled the cycle; polling then
	// pauses until the wait GitHub asked for has passed.
	RateLimited bool
}

func (p PollStats) result() map[string]any {
	return map[string]any{"repos": p.Repos, "synced": p.Synced, "failed": p.Failed, "rate_limited": p.RateLimited}
}

// pollTarget is one registered repository in one workspace.
type pollTarget struct {
	workspaceID pgtype.UUID
	// projectID is the project that registered the repository first; new
	// issues from GitHub land there.
	projectID pgtype.UUID
	name      repoName
	// lastPolled orders the cycle: never-polled first, then oldest.
	lastPolled time.Time
}

// pollGate pauses polling after GitHub throttled it.
type pollGate struct {
	mu          sync.Mutex
	pausedUntil time.Time
}

func (g *pollGate) paused(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return now.Before(g.pausedUntil)
}

func (g *pollGate) pause(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until.After(g.pausedUntil) {
		g.pausedUntil = until
	}
}

// errStopCycle ends the whole cycle: the token is unusable or throttled, so
// every other repository would fail the same way.
type errStopCycle struct{ err error }

func (e *errStopCycle) Error() string { return e.err.Error() }
func (e *errStopCycle) Unwrap() error { return e.err }

// PollOnce runs one cycle over every registered repository. It sends nothing
// while the token is unusable or while a rate-limit pause is in effect.
func (s *Service) PollOnce(ctx context.Context) (PollStats, error) {
	var stats PollStats
	if s == nil {
		return stats, ErrFeatureOff
	}
	if s.db == nil {
		return stats, ErrNoDatabase
	}
	if !s.Usable() || s.pollGate.paused(s.now()) {
		return stats, nil
	}
	targets, err := s.pollTargets(ctx)
	if err != nil {
		return stats, err
	}
	stats.Repos = len(targets)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan pollTarget)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range pollWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				synced, err := s.pollRepo(ctx, t)
				mu.Lock()
				stats.Synced += synced
				if err != nil {
					stats.Failed++
				}
				mu.Unlock()
				var stop *errStopCycle
				if errors.As(err, &stop) {
					cancel(stop)
				}
			}
		}()
	}
feed:
	for _, t := range targets {
		select {
		case work <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()

	var limited *ghsnapshot.RateLimitError
	if cause := context.Cause(ctx); errors.As(cause, &limited) {
		stats.RateLimited = true
		s.pollGate.pause(s.now().Add(limited.RetryAfter))
		s.logger.Warn("github issues poll rate limited; pausing", "retry_after", limited.RetryAfter)
	}
	return stats, nil
}

// pollTargets lists the registered repositories of every workspace, one per
// workspace and name, least recently polled first. Paused repositories are
// left out.
func (s *Service) pollTargets(ctx context.Context) ([]pollTarget, error) {
	q := db.New(s.db)
	resources, err := q.ListGitHubRepoResources(ctx)
	if err != nil {
		return nil, fmt.Errorf("githubissues: list registered repositories: %w", err)
	}
	pins, err := q.ListAllGithubRepoSyncs(ctx)
	if err != nil {
		return nil, fmt.Errorf("githubissues: list repository poll state: %w", err)
	}
	var targets []pollTarget
	for _, res := range resources {
		name, ok := repoNameFromResourceRef(res.ResourceRef)
		if !ok {
			continue
		}
		duplicate := false
		for _, t := range targets {
			if t.workspaceID == res.WorkspaceID && t.name.sameAs(name) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		t := pollTarget{workspaceID: res.WorkspaceID, projectID: res.ProjectID, name: name}
		paused := false
		for _, pin := range pins {
			if pin.WorkspaceID == res.WorkspaceID && name.sameAs(repoName{owner: pin.Owner, name: pin.Name}) {
				paused = pin.Status == "paused"
				t.lastPolled = pin.LastPolledAt.Time
			}
		}
		if !paused {
			targets = append(targets, t)
		}
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].lastPolled.Before(targets[j].lastPolled) })
	return targets, nil
}

// pollRepo reads the issues stream, then the comments stream, of one
// repository. Comments are read only once the issues stream has caught up, so
// a comment never arrives before the issue it belongs to has been imported.
// It returns how many issues and comments it stored or changed.
func (s *Service) pollRepo(ctx context.Context, t pollTarget) (int, error) {
	repo, err := s.RepoClient(ctx, t.workspaceID, t.name.owner, t.name.name)
	switch {
	case errors.Is(err, ErrRepoNotRegistered), errors.Is(err, ErrRepoAmbiguous):
		// Removed since the target list was read, or refused and already
		// logged by RepoClient.
		return 0, nil
	case err != nil:
		return 0, s.repoFailure(t.name.String(), err)
	}
	q := db.New(s.db)
	pin, err := q.GetGithubRepoSync(ctx, db.GetGithubRepoSyncParams{WorkspaceID: t.workspaceID, RepoID: repo.ID()})
	if err != nil {
		return 0, fmt.Errorf("githubissues: read poll state: %w", err)
	}

	synced, caughtUp, err := s.pollIssues(ctx, repo, t.projectID, pin.IssuesCursor)
	if err == nil && caughtUp {
		var n int
		n, err = s.pollComments(ctx, repo, pin.CommentsCursor)
		synced += n
		if err == nil {
			// Writes from Multica the streams just read past are settled
			// here, so a write is only given up after a poll that would
			// have seen it.
			err = s.reconcileIntents(ctx, repo, t.projectID)
		}
	}
	// A cycle cut short (throttled elsewhere, or out of time) says nothing
	// about this repository, so it leaves no error behind.
	var lastError pgtype.Text
	if err != nil && ctx.Err() == nil {
		lastError = pgtype.Text{String: err.Error(), Valid: true}
	}
	// Recorded on a fresh context, so a cycle cut short still records the poll.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ferr := q.FinishGithubRepoPoll(finishCtx, db.FinishGithubRepoPollParams{
		WorkspaceID: t.workspaceID, RepoID: repo.ID(), LastError: lastError,
	}); ferr != nil {
		s.logger.Warn("github issues poll: record poll result", "repo", repo, "error", ferr)
	}
	if err != nil {
		return synced, s.repoFailure(repo.FullName(), err)
	}
	return synced, nil
}

// repoFailure logs a failed repository poll and turns the failures that
// concern every repository into errStopCycle.
func (s *Service) repoFailure(repo string, err error) error {
	var limited *ghsnapshot.RateLimitError
	if errors.As(err, &limited) || errors.Is(err, ErrTokenNotUsable) {
		return &errStopCycle{err: err}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	s.logger.Warn("github issues poll failed", "repo", repo, "error", err)
	return err
}

// stream is one cursor-driven GitHub list: issues or comments.
type stream struct {
	// path is the list endpoint under the repository, without a query.
	path string
	// comments selects which cursor of github_repo_sync the stream moves.
	comments bool
}

func (st stream) cursorParams(repo *Repo, newest time.Time) db.AdvanceGithubRepoSyncCursorsParams {
	p := db.AdvanceGithubRepoSyncCursorsParams{WorkspaceID: repo.workspaceID, RepoID: repo.id}
	if st.comments {
		p.CommentsCursor = pgtype.Timestamptz{Time: newest, Valid: true}
	} else {
		p.IssuesCursor = pgtype.Timestamptz{Time: newest, Valid: true}
	}
	return p
}

// streamKey names one stream of one repository in one workspace.
type streamKey struct {
	workspaceID pgtype.UUID
	repoID      int64
	comments    bool
}

// streamResume is where a stream that stopped before its end asks next: the
// "since" and page number of its next request. The cursor alone cannot say
// this, as it is only a timestamp: a run of items sharing one second that is
// longer than a cycle's page budget would otherwise be re-read from its start
// every cycle and never left. Kept in memory: after a restart, or on a
// replica that has not read the stream yet, the stream starts over from the
// cursor once and resumes from there on.
type streamResume struct {
	since time.Time
	page  int
}

// resumeStore holds the streamResume of every stream that stopped early.
type resumeStore struct {
	mu sync.Mutex
	at map[streamKey]streamResume
}

func (r *resumeStore) get(key streamKey) (streamResume, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.at[key]
	return at, ok
}

func (r *resumeStore) set(key streamKey, at streamResume) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at == nil {
		r.at = map[streamKey]streamResume{}
	}
	r.at[key] = at
}

func (r *resumeStore) clear(key streamKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.at, key)
}

// streamItem is the part of every listed item the stream itself reads.
type streamItem struct {
	UpdatedAt time.Time `json:"updated_at"`
}

// readStream pages through a stream in ascending updated_at order from
// cursor minus pollOverlap, handing each page to handle and moving the cursor
// to the largest updated_at handle accepted. handle returns how many leading
// items of the page it processed; fewer than the page ends the stream at
// that item with handle's error, so a failed item is read again next cycle.
//
// Paging is by cursor rather than page number: each next request asks for
// items updated since the last one seen (less a second, as GitHub timestamps
// have second precision and "since" may be exclusive), so an item updated
// while the stream is read moves behind the read position instead of shifting
// an unread item onto an already-read page. Only when a whole page shares
// that second does the stream step to the next page number.
//
// A stream that stops before its end (page budget spent, cycle out of time,
// an item failed) records where its next request would have gone, and the
// next cycle continues there instead of from cursor minus pollOverlap, so a
// stream always moves forward however long a run of one second is. A
// recorded position behind the overlap window (another replica has read
// further since) is dropped in favor of the cursor.
//
// caughtUp is true when the stream reached its end within the page budget.
func (s *Service) readStream(ctx context.Context, repo *Repo, st stream, cursor pgtype.Timestamptz,
	handle func(items []json.RawMessage) (int, error)) (caughtUp bool, err error) {
	var since time.Time
	if cursor.Valid {
		since = cursor.Time.Add(-pollOverlap)
	}
	page := 1
	key := streamKey{workspaceID: repo.workspaceID, repoID: repo.id, comments: st.comments}
	if at, ok := s.pollResume.get(key); ok && !at.since.Before(since) {
		since, page = at.since, at.page
	}
	defer func() {
		if caughtUp {
			s.pollResume.clear(key)
		} else {
			s.pollResume.set(key, streamResume{since: since, page: page})
		}
	}()
	perPage, pages := s.pollPageSize(), s.pollPageBudget()
	q := db.New(s.db)
	for range pages {
		query := url.Values{
			"sort": {"updated"}, "direction": {"asc"},
			"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)},
		}
		if !st.comments {
			query.Set("state", "all")
		}
		if !since.IsZero() {
			query.Set("since", since.UTC().Format(time.RFC3339))
		}
		resp, err := repo.Do(ctx, http.MethodGet, st.path+"?"+query.Encode(), nil)
		if err != nil {
			return false, err
		}
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("github GET %s: unexpected status %d", st.path, resp.StatusCode)
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("github GET %s: malformed response", st.path)
		}
		items := make([]streamItem, len(raw))
		for i, r := range raw {
			if err := json.Unmarshal(r, &items[i]); err != nil || items[i].UpdatedAt.IsZero() {
				return false, fmt.Errorf("github GET %s: item without updated_at", st.path)
			}
		}

		done, handleErr := handle(raw)
		var newest time.Time
		for _, item := range items[:done] {
			if item.UpdatedAt.After(newest) {
				newest = item.UpdatedAt
			}
		}
		if !newest.IsZero() {
			if _, err := q.AdvanceGithubRepoSyncCursors(ctx, st.cursorParams(repo, newest)); err != nil {
				return false, fmt.Errorf("githubissues: advance cursor: %w", err)
			}
		}
		if handleErr != nil {
			return false, handleErr
		}
		if len(items) < perPage {
			return true, nil
		}
		if next := newest.Add(-time.Second); next.After(since) {
			since, page = next, 1
		} else {
			page++
		}
	}
	return false, nil
}

// pollPageSize and pollPageBudget are pollPerPage and pollPagesPerStream
// unless a test made them smaller.
func (s *Service) pollPageSize() int {
	if s.pollLimits.perPage > 0 {
		return s.pollLimits.perPage
	}
	return pollPerPage
}

func (s *Service) pollPageBudget() int {
	if s.pollLimits.pages > 0 {
		return s.pollLimits.pages
	}
	return pollPagesPerStream
}
