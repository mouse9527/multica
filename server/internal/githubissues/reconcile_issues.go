package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// issueCheckInterval is how often each tracked issue is read on its own:
	// the "since" lists never return an issue deleted or transferred away, so
	// only a read of the issue itself finds out.
	issueCheckInterval = 24 * time.Hour
	// issueChecksPerCycle bounds the issues one repository checks in one poll
	// cycle; the rest wait for the next cycles.
	issueChecksPerCycle = 10

	// goneReasonPrefix starts the issue_external.state_reason values that mark
	// an issue GitHub no longer has where it was tracked. GitHub's own reasons
	// never start with it, and it needs no change to the table.
	goneReasonPrefix = "multica:"
	goneDeleted      = goneReasonPrefix + "deleted"
	goneTransferred  = goneReasonPrefix + "transferred"
)

// issueGone reports whether the reconcile found the issue deleted or
// transferred away on GitHub. Such an issue stays in Multica with its
// identifier but is no longer synced: reads skip it and writes stay local.
func issueGone(ext db.IssueExternal) bool {
	return strings.HasPrefix(ext.StateReason.String, goneReasonPrefix)
}

// issueLocation is the API path GitHub redirects a transferred issue to.
var issueLocation = regexp.MustCompile(`^/repositories/(\d+)/issues/(\d+)$`)

// reconcileIssues is the daily reconcile of one repository, run after a poll
// of it caught up, so the repository itself answered just now and a missing
// issue is missing, not unreachable. Every tracked issue not synced in the
// last issueCheckInterval is read with GET /issues/{n}, a few per cycle:
//
//   - 200: stored like a poll would (and its sync time moves on);
//   - 404 or 410: marked deleted;
//   - 301 to an issue in a repository the workspace registers: the issue is
//     re-attached there; otherwise it is marked transferred.
//
// Deleted and transferred issues are never re-read. Only tracked issues are
// read, never a list of the whole repository (only imported issues are
// followed, see the 2026-11-11 decision on #24), so the requests grow with the
// tracked issues alone: one per issue a day.
func (s *Service) reconcileIssues(ctx context.Context, repo *Repo, projectID pgtype.UUID) (int, error) {
	q := db.New(s.db)
	due, err := q.ListGitHubIssuesToReconcile(ctx, db.ListGitHubIssuesToReconcileParams{
		WorkspaceID: repo.workspaceID, ExternalRepoID: repo.id,
		CheckSeconds: issueCheckInterval.Seconds(),
		MaxRows:      issueChecksPerCycle,
	})
	if err != nil {
		return 0, fmt.Errorf("githubissues: list issues to reconcile: %w", err)
	}
	changed := 0
	for _, ext := range due {
		c, err := s.reconcileIssue(ctx, repo, projectID, ext)
		if err != nil {
			return changed, fmt.Errorf("reconcile %s#%d: %w", repo.FullName(), ext.ExternalNumber, err)
		}
		if c {
			changed++
		}
	}
	if len(due) > 0 && len(due) < issueChecksPerCycle {
		// This cycle checked the last due issue: a pass is complete.
		if err := q.MarkGithubRepoSyncReconciled(ctx, db.MarkGithubRepoSyncReconciledParams{
			WorkspaceID: repo.workspaceID, RepoID: repo.id,
			LastFullReconcileAt: pgtype.Timestamptz{Time: s.now(), Valid: true},
		}); err != nil {
			return changed, fmt.Errorf("githubissues: record reconcile: %w", err)
		}
	}
	return changed, nil
}

// reconcileIssue reads one tracked issue from GitHub and settles what it
// finds. changed is whether the local copy changed.
func (s *Service) reconcileIssue(ctx context.Context, repo *Repo, projectID pgtype.UUID, ext db.IssueExternal) (bool, error) {
	path := "/issues/" + strconv.Itoa(int(ext.ExternalNumber))
	resp, err := repo.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var gh githubIssue
		if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.Number != ext.ExternalNumber {
			return false, fmt.Errorf("github GET %s: malformed issue", path)
		}
		changed, err := s.syncIssue(ctx, repo, projectID, gh, false)
		if err != nil {
			return false, err
		}
		// syncIssue writes nothing for a snapshot older than the stored one;
		// the issue was still read, so its next check is a day away.
		return changed, s.touchSynced(ctx, ext)
	case http.StatusNotFound, http.StatusGone:
		return s.markIssueGone(ctx, ext, goneDeleted)
	case http.StatusMovedPermanently:
		return s.followTransfer(ctx, repo, projectID, ext, resp.Header.Get("Location"))
	default:
		return false, fmt.Errorf("github GET %s: unexpected status %d", path, resp.StatusCode)
	}
}

// followTransfer handles the redirect GitHub answers for a transferred issue.
// When the new repository is registered in the workspace (and pinned to the
// id the redirect names), the issue is re-attached to its new repository and
// number and refreshed from there. Otherwise, or when the new place is
// already tracked by another local issue, it is marked transferred.
func (s *Service) followTransfer(ctx context.Context, repo *Repo, projectID pgtype.UUID, ext db.IssueExternal, location string) (bool, error) {
	toRepoID, toNumber, ok := parseIssueLocation(location)
	if !ok {
		return s.markIssueGone(ctx, ext, goneTransferred)
	}
	target, err := s.repoByID(ctx, repo, toRepoID)
	switch {
	case errors.Is(err, ErrRepoNotRegistered), errors.Is(err, ErrRepoAmbiguous):
		return s.markIssueGone(ctx, ext, goneTransferred)
	case err != nil:
		return false, err
	}
	path := "/issues/" + strconv.Itoa(int(toNumber))
	resp, err := target.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone, http.StatusMovedPermanently:
		// Gone again, or moved on: not followed further.
		return s.markIssueGone(ctx, ext, goneTransferred)
	default:
		return false, fmt.Errorf("github GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var gh githubIssue
	if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.Number != toNumber {
		return false, fmt.Errorf("github GET %s: malformed issue", path)
	}
	moved, err := s.moveIssue(ctx, repo, target, ext, toNumber)
	if err != nil {
		return false, err
	}
	if !moved {
		return s.markIssueGone(ctx, ext, goneTransferred)
	}
	s.logger.Info("github issue transferred; re-attached",
		"issue_id", util.UUIDToString(ext.IssueID),
		"from", fmt.Sprintf("%s#%d", repo.FullName(), ext.ExternalNumber),
		"to", fmt.Sprintf("%s#%d", target.FullName(), toNumber))
	if _, err := s.syncIssue(ctx, target, projectID, gh, false); err != nil {
		return true, fmt.Errorf("sync %s#%d: %w", target.FullName(), toNumber, err)
	}
	return true, s.touchSynced(ctx, ext)
}

// touchSynced starts the next issueCheckInterval of the issue now.
func (s *Service) touchSynced(ctx context.Context, ext db.IssueExternal) error {
	return db.New(s.db).TouchIssueExternalSynced(ctx, db.TouchIssueExternalSyncedParams{
		IssueID: ext.IssueID, WorkspaceID: ext.WorkspaceID,
	})
}

// parseIssueLocation reads the repository id and issue number out of the
// Location of GitHub's redirect for a transferred issue.
func parseIssueLocation(location string) (repoID int64, number int32, ok bool) {
	u, err := url.Parse(location)
	if err != nil {
		return 0, 0, false
	}
	m := issueLocation.FindStringSubmatch(u.Path)
	if m == nil {
		return 0, 0, false
	}
	repoID, err = strconv.ParseInt(m[1], 10, 64)
	if err != nil || repoID <= 0 {
		return 0, 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	return repoID, int32(n), true
}

// repoByID returns the client of the repository pinned to id in from's
// workspace, through RepoClient so the allowlist still decides. A repository
// the workspace never pinned is not registered.
func (s *Service) repoByID(ctx context.Context, from *Repo, id int64) (*Repo, error) {
	if id == from.id {
		return from, nil
	}
	pin, err := db.New(s.db).GetGithubRepoSync(ctx, db.GetGithubRepoSyncParams{WorkspaceID: from.workspaceID, RepoID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRepoNotRegistered
	}
	if err != nil {
		return nil, fmt.Errorf("githubissues: read repository pin: %w", err)
	}
	target, err := s.RepoClient(ctx, from.workspaceID, pin.Owner, pin.Name)
	if err != nil {
		return nil, err
	}
	if target.ID() != id {
		return nil, ErrRepoNotRegistered
	}
	return target, nil
}

// moveIssue re-attaches the issue to number in target, under the locks of
// both places so no other writer of either runs meanwhile. moved is false
// when another local issue already tracks the new place.
func (s *Service) moveIssue(ctx context.Context, from, target *Repo, ext db.IssueExternal, number int32) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	for _, key := range []string{issueLockKey(from, ext.ExternalNumber), issueLockKey(target, number)} {
		if err := q.LockIssueDuplicateKey(ctx, key); err != nil {
			return false, err
		}
	}
	n, err := q.MoveIssueExternal(ctx, db.MoveIssueExternalParams{
		ToRepoID: target.id, ToRepo: target.FullName(), ToNumber: number,
		IssueID: ext.IssueID, WorkspaceID: ext.WorkspaceID,
		FromRepoID: ext.ExternalRepoID, FromNumber: ext.ExternalNumber,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Moved or deleted by someone else meanwhile: nothing left to do here,
		// and the caller's sync finds the issue wherever it is now.
		return true, nil
	}
	return true, tx.Commit(ctx)
}

// markIssueGone marks the issue deleted or transferred, under its lock so a
// write to it in flight finishes first.
func (s *Service) markIssueGone(ctx context.Context, ext db.IssueExternal, reason string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	if err := q.LockIssueDuplicateKey(ctx, issueLockKeyFor(ext.WorkspaceID, ext.ExternalRepoID, ext.ExternalNumber)); err != nil {
		return false, err
	}
	n, err := q.MarkGitHubIssueGone(ctx, db.MarkGitHubIssueGoneParams{
		StateReason: pgtype.Text{String: reason, Valid: true},
		IssueID:     ext.IssueID, WorkspaceID: ext.WorkspaceID,
		ExternalRepoID: ext.ExternalRepoID, ExternalNumber: ext.ExternalNumber,
	})
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if n > 0 {
		s.logger.Warn("github issue no longer on GitHub; no longer synced",
			"issue_id", util.UUIDToString(ext.IssueID),
			"github", fmt.Sprintf("%s#%d", ext.ExternalRepo, ext.ExternalNumber), "reason", reason)
	}
	return n > 0, nil
}
