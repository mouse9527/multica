package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	// ErrNotGitHubIssue: the issue is local, so there is nothing to sync.
	ErrNotGitHubIssue = errors.New("githubissues: the issue is not a GitHub issue")
	// ErrRepoChanged: the registered name of the issue's repository now leads
	// to another repository than the one the issue came from.
	ErrRepoChanged = errors.New("githubissues: the registered repository name now leads to another repository")
	// ErrSyncIncomplete: the issue has more comments than one sync reads.
	ErrSyncIncomplete = errors.New("githubissues: not all comments of the issue were read")
)

// syncTimelineBudget bounds the timeline pages and pull request reads of one
// manual sync, as a guard against a timeline that never ends.
const syncTimelineBudget = 30

// SyncResult is what one manual sync of a GitHub issue changed.
type SyncResult struct {
	IssueID string `json:"issue_id"`
	// IssueUpdated: the title, description, state, labels or status changed.
	IssueUpdated bool `json:"issue_updated"`
	// NewComments counts comments that reached Multica; CommentsUpdated
	// counts edits to comments it already had.
	NewComments     int `json:"new_comments"`
	CommentsUpdated int `json:"comments_updated"`
	// PullsUpdated counts the linked pull requests stored or changed.
	PullsUpdated int `json:"pulls_updated"`
	// Gone is "deleted" or "transferred" when GitHub no longer has the issue
	// where it was tracked; it is then no longer synced.
	Gone string `json:"gone,omitempty"`
	// RequestsUsed is how many GitHub requests the sync sent.
	RequestsUsed int `json:"requests_used"`
	// RateLimit is the quota GitHub reported after the sync, when it did.
	RateLimit *Quota `json:"rate_limit,omitempty"`
}

// SyncIssue reads one GitHub issue now and stores what changed: the issue
// (title, description, state, labels), every comment, the writes from Multica
// still to settle, and the pull requests its timeline links. It runs in the
// caller's request rather than in the background, is not held back by the
// on-view refresh interval, and is idempotent: comments are matched by their
// GitHub id, so syncing again, or after a write from Multica, stores nothing
// twice.
//
// It is refused while GitHub asked to wait (*ghsnapshot.RateLimitError) or
// while the quota is below the guard (*QuotaLowError): before any request, and
// again before each of its reads, against the quota the latest answer
// reported. A sync stopped midway keeps what it stored, and result reports it.
//
// An issue GitHub answers 404 or 410 for, in a repository that still answers,
// is marked deleted; one it redirects is followed as the daily reconcile
// does.
func (s *Service) SyncIssue(ctx context.Context, issue db.Issue) (SyncResult, error) {
	result := SyncResult{IssueID: util.UUIDToString(issue.ID)}
	if s == nil {
		return result, ErrFeatureOff
	}
	if s.db == nil {
		return result, ErrNoDatabase
	}
	q := db.New(s.db)
	ext, err := q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotGitHubIssue
	}
	if err != nil {
		return result, fmt.Errorf("githubissues: read issue link: %w", err)
	}
	if ext.Provider != issuekey.ProviderGitHub {
		return result, ErrNotGitHubIssue
	}
	if issueGone(ext) {
		result.Gone = strings.TrimPrefix(ext.StateReason.String, goneReasonPrefix)
		return result, nil
	}
	if err := s.checkSyncQuota(); err != nil {
		return result, err
	}

	ctx, requests := withManualSync(ctx)
	err = s.syncIssueNow(ctx, issue, ext, &result)
	s.pauseIfThrottled(err)
	result.RequestsUsed = int(requests.Load())
	result.RateLimit = s.QuotaStatus()
	return result, err
}

func (s *Service) syncIssueNow(ctx context.Context, issue db.Issue, ext db.IssueExternal, result *SyncResult) error {
	owner, name, _ := strings.Cut(ext.ExternalRepo, "/")
	repo, err := s.RepoClient(ctx, issue.WorkspaceID, owner, name)
	if err != nil {
		return err
	}
	if repo.ID() != ext.ExternalRepoID {
		return ErrRepoChanged
	}

	path := "/issues/" + strconv.Itoa(int(ext.ExternalNumber))
	resp, err := repo.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var gh githubIssue
		if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.Number != ext.ExternalNumber {
			return fmt.Errorf("github GET %s: malformed issue", path)
		}
		if result.IssueUpdated, err = s.syncIssue(ctx, repo, issue.ProjectID, gh, false); err != nil {
			return fmt.Errorf("sync %s#%d: %w", repo.FullName(), ext.ExternalNumber, err)
		}
		// The daily reconcile need not read it again today.
		if err := s.touchSynced(ctx, ext); err != nil {
			return err
		}
	case http.StatusNotFound, http.StatusGone:
		// A repository the token can no longer read answers 404 for every
		// issue in it; only a repository that still answers says the issue
		// is gone.
		if repoResp, err := repo.Do(ctx, http.MethodGet, "", nil); err != nil {
			return err
		} else if repoResp.StatusCode != http.StatusOK {
			return fmt.Errorf("github GET %s: status %d, and the repository answers %d", path, resp.StatusCode, repoResp.StatusCode)
		}
		if _, err := s.markIssueGone(ctx, ext, goneDeleted); err != nil {
			return err
		}
		result.IssueUpdated, result.Gone = true, strings.TrimPrefix(goneDeleted, goneReasonPrefix)
		return nil
	case http.StatusMovedPermanently:
		if _, err := s.followTransfer(ctx, repo, issue.ProjectID, ext, resp.Header.Get("Location")); err != nil {
			return err
		}
		result.IssueUpdated = true
		// Read where the transfer left the issue: re-attached, or gone.
		if ext, err = db.New(s.db).GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return fmt.Errorf("githubissues: read issue link: %w", err)
		}
		if issueGone(ext) {
			result.Gone = strings.TrimPrefix(ext.StateReason.String, goneReasonPrefix)
			return nil
		}
		if repo, err = s.repoByID(ctx, repo, ext.ExternalRepoID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("github GET %s: unexpected status %d", path, resp.StatusCode)
	}

	if err := s.syncCommentsNow(ctx, repo, issue, ext.ExternalNumber, result); err != nil {
		return err
	}
	// Run after the comments are read, so a write GitHub accepted is already
	// copied when its intent is marked applied.
	if err := s.reconcileIssueIntents(ctx, repo, issue.ProjectID, issue.ID); err != nil {
		return err
	}
	if s.pullMirror == nil {
		return nil
	}
	return s.syncLinkedPullsNow(ctx, repo, ext, result)
}

// syncCommentsNow reads every comment of the issue. New comments are told
// apart from edits by the local count, which only grows by what was stored.
func (s *Service) syncCommentsNow(ctx context.Context, repo *Repo, issue db.Issue, number int32, result *SyncResult) error {
	q := db.New(s.db)
	countParams := db.CountCommentsParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID}
	before, err := q.CountComments(ctx, countParams)
	if err != nil {
		return fmt.Errorf("githubissues: count comments: %w", err)
	}
	complete, changed, err := s.syncIssueComments(ctx, repo, issue.ID, number, importCommentPages)
	after, countErr := q.CountComments(ctx, countParams)
	if countErr == nil {
		result.NewComments = max(int(after-before), 0)
		result.CommentsUpdated = max(changed-result.NewComments, 0)
	}
	switch {
	case err != nil:
		return err
	case countErr != nil:
		return fmt.Errorf("githubissues: count comments: %w", countErr)
	case !complete:
		return fmt.Errorf("%w: more than %d pages", ErrSyncIncomplete, importCommentPages)
	}
	return nil
}

// syncLinkedPullsNow reads the issue's timeline and mirrors the pull requests
// on it that close a tracked issue, as the poller does (pollLinkedPulls).
func (s *Service) syncLinkedPullsNow(ctx context.Context, repo *Repo, ext db.IssueExternal, result *SyncResult) error {
	tracked := db.ListTrackedGitHubIssuesInRepoRow{
		IssueID: ext.IssueID, ExternalNumber: ext.ExternalNumber,
		State: ext.State, ExternalUpdatedAt: ext.ExternalUpdatedAt,
	}
	budget := syncTimelineBudget
	for page := 1; ; page++ {
		n, more, err := s.scanTimelinePage(ctx, repo, tracked, page, &budget)
		result.PullsUpdated += n
		if errors.Is(err, errLinkedBudget) {
			s.logger.Warn("github issue sync: timeline longer than one sync reads", "repo", repo,
				"issue_number", ext.ExternalNumber, "issue_id", util.UUIDToString(ext.IssueID))
			return nil
		}
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	s.linkedPulls.markScanned(ext.IssueID, linkedScan{at: s.now(), issueUpdatedAt: ext.ExternalUpdatedAt.Time})
	return nil
}

// ProjectGitHubIssue is a tracked GitHub issue of a project.
type ProjectGitHubIssue struct {
	IssueID    pgtype.UUID
	Identifier string
	State      string
}

// ListProjectGitHubIssues lists the GitHub issues of a project that a manual
// sync of the project reads, by repository and number; openOnly leaves out
// those closed on GitHub. It sends nothing to GitHub.
func (s *Service) ListProjectGitHubIssues(ctx context.Context, workspaceID, projectID pgtype.UUID, openOnly bool) ([]ProjectGitHubIssue, error) {
	if s == nil {
		return nil, ErrFeatureOff
	}
	if s.db == nil {
		return nil, ErrNoDatabase
	}
	rows, err := db.New(s.db).ListProjectGitHubIssues(ctx, db.ListProjectGitHubIssuesParams{
		WorkspaceID: workspaceID, ProjectID: projectID, OpenOnly: openOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("githubissues: list project issues: %w", err)
	}
	out := make([]ProjectGitHubIssue, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProjectGitHubIssue{
			IssueID:    row.IssueID,
			Identifier: row.ExternalRepo + "#" + strconv.Itoa(int(row.ExternalNumber)),
			State:      row.State,
		})
	}
	return out, nil
}
