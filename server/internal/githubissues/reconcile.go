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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// reconcileBatch bounds the intents one repository settles per cycle.
	reconcileBatch = 20
	// reconcilePages bounds the list pages read to find one unknown create.
	reconcilePages = 3
)

// reconcileIntents settles the writes of one repository that the polls have
// not settled, after a poll of the repository caught up:
//
//   - github_ok (GitHub accepted, local copy failed), one cycle after the
//     write: a missing new issue or comment is fetched and adopted; an edit
//     has already been copied by the poll; a deleted comment's local copy is
//     deleted through the delete path (the comments list never returns a
//     deleted comment, so the poll cannot).
//   - pending or unknown (GitHub never answered), after the grace period
//     (unknownGrace): the write is looked for on GitHub, a new issue or
//     comment by the op in its marker, an edit or a deletion by comparing
//     GitHub's current state. Found, it is adopted, applied or (a deletion)
//     finished locally; not found, it is handed to a person
//     ("needs_confirmation: ...") and never retried.
func (s *Service) reconcileIntents(ctx context.Context, repo *Repo, projectID pgtype.UUID) error {
	q := db.New(s.db)
	intents, err := q.ListGithubWriteIntentsToReconcile(ctx, db.ListGithubWriteIntentsToReconcileParams{
		WorkspaceID: repo.workspaceID, RepoID: repo.id,
		// An accepted write heals on the next poll; the delay keeps it clear
		// of a local write still in flight.
		AcceptedBefore:   pgtype.Timestamptz{Time: s.now().Add(-pollInterval), Valid: true},
		UnansweredBefore: pgtype.Timestamptz{Time: s.now().Add(-unknownGrace), Valid: true},
		RowLimit:         reconcileBatch,
	})
	if err != nil {
		return fmt.Errorf("githubissues: list write intents: %w", err)
	}
	for _, intent := range intents {
		if err := s.reconcileIntent(ctx, repo, projectID, intent); err != nil {
			var limited *ghsnapshot.RateLimitError
			if errors.As(err, &limited) || errors.Is(err, ErrTokenNotUsable) || ctx.Err() != nil {
				return err
			}
			// One intent's trouble does not hold back the others; it is
			// looked at again next cycle.
			s.logger.Warn("github write reconcile failed", "intent_id", intent.ID, "op", intent.Op, "repo", repo, "error", err)
		}
	}
	return nil
}

func (s *Service) reconcileIntent(ctx context.Context, repo *Repo, projectID pgtype.UUID, intent db.GithubWriteIntent) error {
	accepted := intent.State == intentGitHubOK
	switch intent.Op {
	case opCreateIssue:
		gh, found, err := s.findCreatedIssue(ctx, repo, intent)
		if err != nil || !found {
			return s.notFound(ctx, intent, err, "the issue was not found on GitHub")
		}
		if _, err := s.syncIssue(ctx, repo, projectID, gh); err != nil {
			return err
		}
		return s.requireSettled(ctx, intent, "the issue is on GitHub but could not be linked")
	case opCreateComment:
		c, found, err := s.findCreatedComment(ctx, repo, intent)
		if err != nil || !found {
			return s.notFound(ctx, intent, err, "the comment was not found on GitHub")
		}
		if handled, _, err := s.syncWrittenComment(ctx, repo, intent.IssueID, intent.ExternalNumber.Int32, c); err != nil || !handled {
			return err
		}
		return s.requireSettled(ctx, intent, "the comment is on GitHub but could not be linked")
	case opUpdateIssue:
		if accepted {
			// GitHub has the edit; the poll that just caught up copied it.
			return s.markApplied(ctx, intent)
		}
		gh, status, err := s.fetchJSON(ctx, repo, "/issues/"+strconv.Itoa(int(intent.ExternalNumber.Int32)))
		if err != nil {
			return err
		}
		var issue githubIssue
		if status == http.StatusOK && json.Unmarshal(gh, &issue) == nil && issueMatchesDigest(issue, intent.PayloadDigest) {
			return s.markApplied(ctx, intent)
		}
		return s.handOver(ctx, intent, "GitHub does not show the edit")
	case opUpdateComment:
		if accepted {
			return s.markApplied(ctx, intent)
		}
		raw, status, err := s.fetchJSON(ctx, repo, "/issues/comments/"+strconv.FormatInt(intent.GithubID.Int64, 10))
		if err != nil {
			return err
		}
		var c githubComment
		if status == http.StatusOK && json.Unmarshal(raw, &c) == nil {
			if m, ok := parseMarker(c.Body); ok && m.op == util.UUIDToString(intent.ID) {
				return s.markApplied(ctx, intent)
			}
		}
		return s.handOver(ctx, intent, "GitHub does not show the edit")
	case opDeleteComment:
		if !accepted {
			_, status, err := s.fetchJSON(ctx, repo, "/issues/comments/"+strconv.FormatInt(intent.GithubID.Int64, 10))
			if err != nil {
				return err
			}
			if status != http.StatusNotFound && status != http.StatusGone {
				return s.handOver(ctx, intent, "the comment is still on GitHub")
			}
		}
		// GitHub no longer has the comment: finish the local delete the
		// write could not. The comments list never returns a deleted
		// comment, so nothing else would.
		return s.deleteLocalComment(ctx, intent)
	}
	return nil
}

// findCreatedIssue looks for the issue a create_issue intent opened: by its
// number when GitHub answered, otherwise by the op in its marker among the
// issues updated since the write.
func (s *Service) findCreatedIssue(ctx context.Context, repo *Repo, intent db.GithubWriteIntent) (githubIssue, bool, error) {
	if intent.ExternalNumber.Valid {
		raw, status, err := s.fetchJSON(ctx, repo, "/issues/"+strconv.Itoa(int(intent.ExternalNumber.Int32)))
		if err != nil || status != http.StatusOK {
			return githubIssue{}, false, err
		}
		var gh githubIssue
		return gh, json.Unmarshal(raw, &gh) == nil, nil
	}
	var found githubIssue
	ok, err := s.scanSince(ctx, repo, "/issues", true, intent.CreatedAt.Time, func(raw json.RawMessage) bool {
		var gh githubIssue
		if json.Unmarshal(raw, &gh) != nil || len(gh.PullRequest) > 0 && string(gh.PullRequest) != "null" {
			return false
		}
		if m, ok := parseMarker(gh.body()); ok && m.op == util.UUIDToString(intent.ID) {
			found = gh
			return true
		}
		return false
	})
	return found, ok, err
}

// findCreatedComment looks for the comment a create_comment intent posted.
func (s *Service) findCreatedComment(ctx context.Context, repo *Repo, intent db.GithubWriteIntent) (githubComment, bool, error) {
	if intent.GithubID.Valid {
		raw, status, err := s.fetchJSON(ctx, repo, "/issues/comments/"+strconv.FormatInt(intent.GithubID.Int64, 10))
		if err != nil || status != http.StatusOK {
			return githubComment{}, false, err
		}
		var c githubComment
		return c, json.Unmarshal(raw, &c) == nil, nil
	}
	var found githubComment
	path := "/issues/" + strconv.Itoa(int(intent.ExternalNumber.Int32)) + "/comments"
	ok, err := s.scanSince(ctx, repo, path, false, intent.CreatedAt.Time, func(raw json.RawMessage) bool {
		var c githubComment
		if json.Unmarshal(raw, &c) != nil {
			return false
		}
		if m, ok := parseMarker(c.Body); ok && m.op == util.UUIDToString(intent.ID) {
			found = c
			return true
		}
		return false
	})
	return found, ok, err
}

// scanSince pages through a list of items updated since the write (less the
// poll overlap, for clock skew between Multica and GitHub) until match
// accepts one.
func (s *Service) scanSince(ctx context.Context, repo *Repo, path string, issues bool, since time.Time, match func(json.RawMessage) bool) (bool, error) {
	for page := 1; page <= reconcilePages; page++ {
		query := url.Values{
			"since":    {since.Add(-pollOverlap).UTC().Format(time.RFC3339)},
			"per_page": {strconv.Itoa(pollPerPage)}, "page": {strconv.Itoa(page)},
		}
		if issues {
			query.Set("state", "all")
			query.Set("sort", "updated")
			query.Set("direction", "asc")
		}
		raw, status, err := s.fetchJSON(ctx, repo, path+"?"+query.Encode())
		if err != nil {
			return false, err
		}
		if status != http.StatusOK {
			return false, fmt.Errorf("github GET %s: unexpected status %d", path, status)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return false, fmt.Errorf("github GET %s: malformed response", path)
		}
		for _, item := range items {
			if match(item) {
				return true, nil
			}
		}
		if len(items) < pollPerPage {
			return false, nil
		}
	}
	return false, nil
}

func (s *Service) fetchJSON(ctx context.Context, repo *Repo, path string) ([]byte, int, error) {
	resp, err := repo.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.StatusCode, nil
}

// notFound hands an intent whose write was not found to a person, unless the
// lookup itself failed (err), in which case it is looked for again next cycle.
func (s *Service) notFound(ctx context.Context, intent db.GithubWriteIntent, err error, reason string) error {
	if err != nil {
		return err
	}
	return s.handOver(ctx, intent, reason)
}

// requireSettled hands an intent to a person if the sync that should have
// adopted its write did not.
func (s *Service) requireSettled(ctx context.Context, intent db.GithubWriteIntent, reason string) error {
	current, err := db.New(s.db).GetGithubWriteIntent(ctx, db.GetGithubWriteIntentParams{ID: intent.ID, WorkspaceID: intent.WorkspaceID})
	if err != nil {
		return err
	}
	if current.State == intentApplied {
		return nil
	}
	return s.handOver(ctx, current, reason)
}

func (s *Service) markApplied(ctx context.Context, intent db.GithubWriteIntent) error {
	_, err := db.New(s.db).UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: intent.ID, WorkspaceID: intent.WorkspaceID, State: intentApplied,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// handOver stops looking for a write and leaves it to a person: the intent
// keeps its state (a pending one becomes unknown), its error starts with
// needs_confirmation, and it is never retried or checked again. The write's
// author was already told the result was unconfirmed when it was made.
func (s *Service) handOver(ctx context.Context, intent db.GithubWriteIntent, reason string) error {
	state := intent.State
	if state == intentPending {
		state = intentUnknown
	}
	_, err := db.New(s.db).UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: intent.ID, WorkspaceID: intent.WorkspaceID, State: state, Error: errorText(needsConfirmation + reason),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err == nil {
		s.logger.Warn("github write needs confirmation; not retried", "intent_id", intent.ID, "op", intent.Op,
			"repo", intent.ExternalRepo, "number", intent.ExternalNumber.Int32, "reason", reason)
	}
	return err
}

// issueMatchesDigest reports whether GitHub's issue holds the fields an
// update_issue intent set, by recomputing the intent's digest from them.
func issueMatchesDigest(gh githubIssue, digest string) bool {
	keys, _, ok := cutDigest(digest)
	if !ok {
		return false
	}
	current := map[string]any{}
	for _, key := range keys {
		switch key {
		case "title":
			current[key] = gh.Title
		case "body":
			current[key] = gh.body()
		case "state":
			current[key] = gh.State
		case "state_reason":
			current[key] = stringValue(gh.StateReason)
		case "labels":
			names := gh.labelNames()
			sort.Strings(names)
			current[key] = names
		default:
			return false
		}
	}
	return payloadDigest(current) == digest
}

func cutDigest(digest string) (keys []string, sum string, ok bool) {
	i := strings.LastIndex(digest, ":")
	if i < 0 {
		return nil, "", false
	}
	if i > 0 {
		keys = strings.Split(digest[:i], ",")
	}
	return keys, digest[i+1:], true
}

// CommentDeleter deletes the local copy of a comment through the same entry
// as a person's delete (DeleteComment), acting as userID, so tasks, events and
// thread tombstones are handled as for any delete. ctx is marked
// WithSyncedWrite: nothing is sent to GitHub.
type CommentDeleter interface {
	DeleteGitHubComment(ctx context.Context, userID pgtype.UUID, comment db.Comment) error
}

// SetCommentDeleter installs the local delete path. It is called once while
// the server is wired, before polling starts.
func (s *Service) SetCommentDeleter(d CommentDeleter) {
	s.commentDeleter = d
}

// deleteLocalComment removes the local copy of a comment GitHub deleted and
// settles its delete_comment intent. A failure is retried next cycle.
func (s *Service) deleteLocalComment(ctx context.Context, intent db.GithubWriteIntent) error {
	q := db.New(s.db)
	comment, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: intent.CommentID, WorkspaceID: intent.WorkspaceID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case !comment.DeletedAt.Valid:
		if s.commentDeleter == nil {
			s.logger.Warn("github issues: no comment delete path; local copy kept", "intent_id", intent.ID)
			return nil
		}
		actor, err := s.statusActor(ctx, intent.WorkspaceID)
		if err != nil {
			return err
		}
		if err := s.commentDeleter.DeleteGitHubComment(WithSyncedWrite(ctx), actor, comment); err != nil {
			return fmt.Errorf("delete local comment: %w", err)
		}
	}
	if err := q.DeleteCommentExternal(ctx, db.DeleteCommentExternalParams{CommentID: intent.CommentID, WorkspaceID: intent.WorkspaceID}); err != nil {
		return err
	}
	s.logger.Info("github comment deletion healed locally", "intent_id", intent.ID, "repo", intent.ExternalRepo)
	return s.markApplied(ctx, intent)
}
