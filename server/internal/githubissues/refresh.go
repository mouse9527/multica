package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// refreshInterval is the least time between two on-view refreshes of
	// one issue.
	refreshInterval = 15 * time.Second
	// refreshTimeout bounds one refresh: the issue and its comment pages.
	refreshTimeout = 30 * time.Second
	// refreshCommentPages bounds the comment pages one refresh reads; the
	// poller picks up anything past them.
	refreshCommentPages = 10
)

// refreshGate admits at most one on-view refresh per issue every
// refreshInterval, and never a second one while the first is still running.
// It is per process: each replica refreshes on its own views.
type refreshGate struct {
	mu       sync.Mutex
	started  map[pgtype.UUID]time.Time
	inFlight map[pgtype.UUID]bool
}

// admit reports whether issueID may refresh at now, and if so records the
// refresh as started. done must follow every admitted refresh.
func (g *refreshGate) admit(issueID pgtype.UUID, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight[issueID] {
		return false
	}
	if at, ok := g.started[issueID]; ok && now.Sub(at) < refreshInterval {
		return false
	}
	if g.started == nil {
		g.started, g.inFlight = map[pgtype.UUID]time.Time{}, map[pgtype.UUID]bool{}
	}
	// Entries past the interval decide nothing any more; dropping them here
	// keeps the table to the issues viewed in the last interval.
	for id, at := range g.started {
		if now.Sub(at) >= refreshInterval && !g.inFlight[id] {
			delete(g.started, id)
		}
	}
	g.started[issueID] = now
	g.inFlight[issueID] = true
	return true
}

func (g *refreshGate) done(issueID pgtype.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inFlight, issueID)
}

// RefreshIssueOnView is called when someone opens an issue or its comments.
// For an issue that mirrors a GitHub issue it reads the issue and its
// comments from GitHub in the background, at most once per issue every
// refreshInterval, and returns at once: the caller answers with what is
// stored, and the changes reach clients through the usual realtime events.
// Local issues send nothing.
func (s *Service) RefreshIssueOnView(ctx context.Context, issue db.Issue) {
	if s == nil || s.db == nil || !s.Usable() || s.pollGate.paused(s.now()) {
		return
	}
	if !s.refreshGate.admit(issue.ID, s.now()) {
		return
	}
	s.refreshing.Add(1)
	go func() {
		defer s.refreshing.Done()
		defer s.refreshGate.done(issue.ID)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		if err := s.refreshIssue(ctx, issue); err != nil {
			var limited *ghsnapshot.RateLimitError
			if errors.As(err, &limited) {
				s.pollGate.pause(s.now().Add(limited.RetryAfter))
			}
			s.logger.Warn("github issues refresh on view failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
		}
	}()
}

// refreshIssue reads one GitHub issue and its comments and stores them the
// way the poller does.
func (s *Service) refreshIssue(ctx context.Context, issue db.Issue) error {
	q := db.New(s.db)
	ext, err := q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("githubissues: read issue link: %w", err)
	}
	if ext.Provider != issuekey.ProviderGitHub {
		return nil
	}
	owner, name, _ := strings.Cut(ext.ExternalRepo, "/")
	repo, err := s.RepoClient(ctx, issue.WorkspaceID, owner, name)
	switch {
	case errors.Is(err, ErrRepoNotRegistered), errors.Is(err, ErrRepoAmbiguous), errors.Is(err, ErrTokenNotUsable):
		// No longer allowed, or nothing can be read now; the issue keeps
		// what is stored.
		return nil
	case err != nil:
		return err
	}
	if repo.ID() != ext.ExternalRepoID {
		// The name now leads to another repository than the one the issue
		// came from.
		return nil
	}
	number := ext.ExternalNumber
	path := "/issues/" + strconv.Itoa(int(number))

	resp, err := repo.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var gh githubIssue
	if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.Number != number {
		return fmt.Errorf("github GET %s: malformed issue", path)
	}
	if len(gh.PullRequest) > 0 && string(gh.PullRequest) != "null" {
		return nil
	}
	if _, err := s.syncIssue(ctx, repo, issue.ProjectID, gh, false); err != nil {
		return fmt.Errorf("sync %s#%d: %w", repo.FullName(), number, err)
	}
	return s.syncIssueComments(ctx, repo, ext.IssueID, number)
}

// syncIssueComments reads the comments of one GitHub issue, up to
// refreshCommentPages pages, and stores them on the local issue issueID.
// Comments past those pages are left to the poller.
func (s *Service) syncIssueComments(ctx context.Context, repo *Repo, issueID pgtype.UUID, number int32) error {
	path := "/issues/" + strconv.Itoa(int(number))
	perPage := s.pollPageSize()
	for page := 1; page <= refreshCommentPages; page++ {
		query := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		resp, err := repo.Do(ctx, http.MethodGet, path+"/comments?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("github GET %s/comments: unexpected status %d", path, resp.StatusCode)
		}
		var comments []githubComment
		if err := json.Unmarshal(resp.Body, &comments); err != nil {
			return fmt.Errorf("github GET %s/comments: malformed response", path)
		}
		for _, c := range comments {
			if c.ID <= 0 || c.UpdatedAt.IsZero() {
				return fmt.Errorf("github GET %s/comments: malformed comment", path)
			}
			if _, err := s.syncComment(ctx, repo, issueID, number, c); err != nil {
				return err
			}
		}
		if len(comments) < perPage {
			return nil
		}
	}
	return nil
}
