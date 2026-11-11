package githubissues

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The poller meets three kinds of content written from Multica:
//
//   - its own echo: the local copy is already linked by GitHub id and only
//     follows later edits made on GitHub;
//   - a write GitHub accepted whose local copy failed (intent github_ok), or
//     whose answer never came (pending, unknown): it is adopted, that is
//     stored under the author recorded in the intent, never imported as an
//     external item a second time;
//   - a body that merely carries a marker someone pasted: shown as external.
//
// trustedIntent tells the second kind from the third.

// trustedIntent returns the intent a body's marker names when the marker can
// be believed: the workspace holds an intent with that op, of one of ops, on
// this repository, written by the token's account, for this GitHub item
// (githubID) or, when GitHub never answered, for this issue (number), and
// naming the same actor as the marker.
func (s *Service) trustedIntent(ctx context.Context, q *db.Queries, repo *Repo, body, login string, githubID int64, number int32, ops ...string) (db.GithubWriteIntent, bool) {
	m, ok := parseMarker(body)
	if !ok {
		return db.GithubWriteIntent{}, false
	}
	// Every Multica write is made with the token, so it is authored by the
	// token's account. A marker on anyone else's text is pasted.
	if account := s.state.login(); account != "" && !strings.EqualFold(login, account) {
		return db.GithubWriteIntent{}, false
	}
	op, err := util.ParseUUID(m.op)
	if err != nil {
		return db.GithubWriteIntent{}, false
	}
	intent, err := q.GetGithubWriteIntent(ctx, db.GetGithubWriteIntentParams{ID: op, WorkspaceID: repo.workspaceID})
	if err != nil {
		return db.GithubWriteIntent{}, false
	}
	known := false
	for _, candidate := range ops {
		known = known || intent.Op == candidate
	}
	switch {
	case !known, intent.RepoID != repo.id, intent.State == intentFailed:
		return db.GithubWriteIntent{}, false
	case intent.ActorType != m.actorType || util.UUIDToString(intent.ActorID) != m.actorID:
		return db.GithubWriteIntent{}, false
	case intent.GithubID.Valid:
		return intent, intent.GithubID.Int64 == githubID
	case intent.State == intentPending || intent.State == intentUnknown:
		// GitHub never answered, so no id was learned; the item must at
		// least belong to the issue the write was aimed at.
		return intent, !intent.ExternalNumber.Valid || intent.ExternalNumber.Int32 == number
	}
	return db.GithubWriteIntent{}, false
}

// adoptIssue stores a GitHub issue opened from Multica whose local create
// never committed. The creator is the intent's actor and the description is
// the body without the marker.
func (s *Service) adoptIssue(ctx context.Context, tx pgx.Tx, q *db.Queries, repo *Repo, projectID pgtype.UUID, gh githubIssue, intent db.GithubWriteIntent) (db.Issue, error) {
	status, err := initialStatus(ctx, q, s.logger, repo.workspaceID, projectID, gh)
	if err != nil {
		return db.Issue{}, err
	}
	number, err := service.AllocateIssueNumber(ctx, q, repo.workspaceID, service.IssueCountPolicy{Action: entitlement.ActionOff})
	if err != nil {
		return db.Issue{}, fmt.Errorf("allocate issue number: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, repo.workspaceID, status)
	if err != nil {
		return db.Issue{}, err
	}
	issue, err := q.CreateIssue(ctx, db.CreateIssueParams{
		ID:          dbid.NewV7(),
		WorkspaceID: repo.workspaceID,
		Title:       NormalizeGitHubText(gh.Title),
		Description: pgtype.Text{String: NormalizeGitHubText(stripMarker(gh.body())), Valid: true},
		Status:      status,
		Priority:    "none",
		CreatorType: intent.ActorType,
		CreatorID:   intent.ActorID,
		Position:    position,
		Number:      number,
		ProjectID:   projectID,
	})
	if err != nil {
		return db.Issue{}, err
	}
	if _, err := q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: intent.ID, WorkspaceID: intent.WorkspaceID, State: intentApplied, IssueID: issue.ID,
		GithubID:       pgtype.Int8{Int64: gh.ID, Valid: gh.ID > 0},
		ExternalNumber: pgtype.Int4{Int32: gh.Number, Valid: true},
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, err
	}
	s.logger.Info("github issue written from Multica healed by poll", "intent_id", intent.ID, "repo", repo, "number", gh.Number)
	return issue, nil
}

// ownIssueDescription is the description of an issue written from Multica,
// as GitHub now holds it: the body without the marker when the marker names
// a write to this issue, otherwise the body as it is.
func ownIssueDescription(ctx context.Context, q *db.Queries, repo *Repo, issueID pgtype.UUID, gh githubIssue) string {
	body := gh.body()
	if markerTargets(ctx, q, repo, body, issueID) {
		body = stripMarker(body)
	}
	return NormalizeGitHubText(body)
}

// markerTargets reports whether a body's marker names a write intent on
// issueID. An edit of the description writes a new marker, and GitHub keeps
// the issue's original author, so the description of an issue opened from
// Multica is matched by issue rather than by author.
func markerTargets(ctx context.Context, q *db.Queries, repo *Repo, body string, issueID pgtype.UUID) bool {
	m, ok := parseMarker(body)
	if !ok {
		return false
	}
	op, err := util.ParseUUID(m.op)
	if err != nil {
		return false
	}
	intent, err := q.GetGithubWriteIntent(ctx, db.GetGithubWriteIntentParams{ID: op, WorkspaceID: repo.workspaceID})
	return err == nil && intent.RepoID == repo.id && intent.IssueID == issueID && intent.State != intentFailed &&
		intent.ActorType == m.actorType && util.UUIDToString(intent.ActorID) == m.actorID
}

// syncWrittenComment handles a GitHub comment written from Multica: the echo
// of a linked comment, or the adoption of one whose local copy is missing. It
// reports false for every other comment, which the caller syncs as external.
//
// Neither path starts an agent: the echo of an agent's own comment must not
// wake it again, and an adopted comment's trigger moment has passed.
func (s *Service) syncWrittenComment(ctx context.Context, repo *Repo, issueID pgtype.UUID, number int32, c githubComment) (handled, changed bool, err error) {
	q := db.New(s.db)
	existing, err := q.GetCommentExternalByExternalID(ctx, db.GetCommentExternalByExternalIDParams{
		WorkspaceID: repo.workspaceID, ExternalID: c.ID,
	})
	switch {
	case err == nil:
		comment, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: existing.CommentID, WorkspaceID: repo.workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return true, false, nil
		}
		if err != nil {
			return true, false, err
		}
		if comment.AuthorType == externalAuthorType {
			return false, false, nil
		}
		changed, err := s.refreshOwnComment(ctx, repo, comment, existing, c)
		return true, changed, err
	case !errors.Is(err, pgx.ErrNoRows):
		return true, false, err
	}
	intent, ok := s.trustedIntent(ctx, q, repo, c.Body, c.User.Login, c.ID, number, opCreateComment)
	if !ok || intent.IssueID != issueID {
		return false, false, nil
	}
	created, err := s.adoptComment(ctx, repo, issueID, intent, c)
	return true, created, err
}

// refreshOwnComment follows an edit made on GitHub to a comment written from
// Multica. GitHub is the only writer, so its text wins; the marker of the
// write that produced it is dropped.
func (s *Service) refreshOwnComment(ctx context.Context, repo *Repo, comment db.Comment, ext db.CommentExternal, c githubComment) (bool, error) {
	content := NormalizeGitHubText(c.Body)
	if _, ok := s.trustedIntent(ctx, db.New(s.db), repo, c.Body, c.User.Login, c.ID, 0, opCreateComment, opUpdateComment); ok {
		content = NormalizeGitHubText(stripMarker(c.Body))
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	// Waits out a write from Multica to this comment, then judges the
	// snapshot against what that write left (writeGuard).
	if err := q.LockIssueDuplicateKey(ctx, commentLockKey(comment.ID)); err != nil {
		return false, err
	}
	if comment, err = q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: comment.ID, WorkspaceID: comment.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if ext, err = q.LockCommentExternalByExternalID(ctx, db.LockCommentExternalByExternalIDParams{
		WorkspaceID: comment.WorkspaceID, ExternalID: c.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if comment.DeletedAt.Valid || ext.CommentID != comment.ID || c.UpdatedAt.Before(ext.ExternalUpdatedAt.Time) {
		return false, nil
	}
	changed := false
	var issueRevision int64
	if content != comment.Content {
		updated, err := q.UpdateComment(ctx, db.UpdateCommentParams{ID: comment.ID, Content: content, SourceTaskID: comment.SourceTaskID})
		if err != nil {
			return false, err
		}
		comment, issueRevision, changed = updated.Comment(), updated.IssueRevision, true
		if _, err := q.DiscardExternalCommentWakeupReceipts(ctx, db.DiscardExternalCommentWakeupReceiptsParams{
			IssueID: comment.IssueID, CommentID: util.UUIDToString(comment.ID),
		}); err != nil {
			return false, fmt.Errorf("discard wakeup receipts: %w", err)
		}
	}
	if _, err := q.UpsertCommentExternal(ctx, db.UpsertCommentExternalParams{
		CommentID: comment.ID, WorkspaceID: comment.WorkspaceID, ExternalID: c.ID,
		Login: SafeLogin(c.User.Login), AvatarUrl: ext.AvatarUrl,
		ExternalUpdatedAt: pgtype.Timestamptz{Time: c.UpdatedAt, Valid: true},
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if changed {
		s.publishWrittenComment(ctx, comment, false, issueRevision)
	}
	return changed, nil
}

// adoptComment stores a comment written from Multica whose local copy is
// missing, with the id chosen before the write, so a local insert racing
// this one cannot produce a second copy.
func (s *Service) adoptComment(ctx context.Context, repo *Repo, issueID pgtype.UUID, intent db.GithubWriteIntent, c githubComment) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	commentID := intent.CommentID
	if !commentID.Valid {
		commentID = dbid.NewV7()
	}
	if err := q.LockIssueDuplicateKey(ctx, commentLockKey(commentID)); err != nil {
		return false, err
	}
	var comment db.Comment
	var issueRevision int64
	created := false
	existing, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: commentID, WorkspaceID: repo.workspaceID})
	switch {
	case err == nil:
		// The local insert committed; only its link was lost.
		comment = existing
	case errors.Is(err, pgx.ErrNoRows):
		row, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: commentID, IssueID: issueID, WorkspaceID: repo.workspaceID,
			AuthorType: intent.ActorType, AuthorID: intent.ActorID,
			Content: NormalizeGitHubText(stripMarker(c.Body)), Type: "comment",
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrIssueNotFound
		}
		if err != nil {
			return false, err
		}
		comment, issueRevision, created = row.Comment(), row.IssueRevision, true
		if _, err := q.DiscardExternalCommentWakeupReceipts(ctx, db.DiscardExternalCommentWakeupReceiptsParams{
			IssueID: issueID, CommentID: util.UUIDToString(comment.ID),
		}); err != nil {
			return false, fmt.Errorf("discard wakeup receipts: %w", err)
		}
	default:
		return false, err
	}
	var avatar pgtype.Text
	if c.User.AvatarURL != "" {
		avatar = pgtype.Text{String: c.User.AvatarURL, Valid: true}
	}
	if _, err := q.UpsertCommentExternal(ctx, db.UpsertCommentExternalParams{
		CommentID: comment.ID, WorkspaceID: repo.workspaceID, ExternalID: c.ID,
		Login: SafeLogin(c.User.Login), AvatarUrl: avatar,
		ExternalUpdatedAt: pgtype.Timestamptz{Time: c.UpdatedAt, Valid: true},
	}); err != nil {
		return false, err
	}
	if _, err := q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: intent.ID, WorkspaceID: intent.WorkspaceID, State: intentApplied,
		GithubID: pgtype.Int8{Int64: c.ID, Valid: true}, CommentID: comment.ID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.logger.Info("github comment written from Multica healed by poll", "intent_id", intent.ID, "repo", repo, "comment_id", c.ID)
	if created {
		s.publishWrittenComment(ctx, comment, true, issueRevision)
	}
	return created, nil
}

// publishWrittenComment broadcasts a comment the poller stored or changed
// under its Multica author, for live timelines. The actor is the platform:
// the author did not act now.
func (s *Service) publishWrittenComment(ctx context.Context, comment db.Comment, created bool, issueRevision int64) {
	if s.bus == nil {
		return
	}
	issue, err := db.New(s.db).GetIssue(ctx, comment.IssueID)
	if err != nil {
		return
	}
	s.publishExternalComment(issue, CommentSyncResult{Comment: comment, Created: created, Changed: !created}, issueRevision)
}
