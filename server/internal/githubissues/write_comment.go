package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// BeginCommentCreate posts a new comment on the GitHub issue. It returns nil
// when the issue is local. The caller creates the local comment with
// Write.CommentID and then calls Finish.
func (s *Service) BeginCommentCreate(ctx context.Context, actor Actor, issue db.Issue, content string) (*Write, error) {
	if s == nil {
		return nil, nil
	}
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	ext, err := q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if issueGone(ext) {
		// Deleted or transferred away on GitHub: the comment stays local.
		return nil, nil
	}
	repo, err := s.repoForIssue(ctx, ext)
	if err != nil {
		return nil, err
	}
	commentID := dbid.NewV7()
	// Held until Finish or Close: the poller adopts a comment under the same
	// lock (adoptComment), so it cannot insert the reserved id before the
	// caller's own insert lands.
	guard, err := s.lockTarget(ctx, commentLockKey(commentID))
	if err != nil {
		return nil, err
	}
	intent, resp, err := s.send(ctx, writeRequest{
		op: opCreateComment, actor: actor, repo: repo, method: http.MethodPost,
		path:    "/issues/" + strconv.Itoa(int(ext.ExternalNumber)) + "/comments",
		issueID: issue.ID, number: ext.ExternalNumber, commentID: commentID,
		payload: func(op pgtype.UUID) map[string]any {
			return map[string]any{"body": signedBody(content, actor, op)}
		},
	})
	if err != nil {
		guard.release()
		return nil, err
	}
	var gh githubComment
	if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.ID <= 0 {
		guard.release()
		s.settleIntent(context.WithoutCancel(ctx), intent, intentUnknown, "malformed create response")
		return nil, &WriteError{Kind: WriteUnknown, msg: unconfirmedMessage}
	}
	w := &Write{svc: s, intent: intent, repo: repo, comment: &gh, guard: guard}
	s.accept(ctx, &w.intent, gh.ID, 0)
	return w, nil
}

// CommentBase is the caller's preconditions on a comment edit
// (content_base, expected_revision). They are checked under the comment's
// lock before anything is sent; a mismatch is a *StaleWriteError.
type CommentBase struct {
	Content          *string
	ExpectedRevision *int64
}

// BeginCommentUpdate rewrites a comment Multica posted on GitHub. It returns
// nil for a comment that never reached GitHub (it has no comment_external
// row) or whose text does not change.
func (s *Service) BeginCommentUpdate(ctx context.Context, actor Actor, comment db.Comment, content string, base CommentBase) (*Write, error) {
	if s == nil || IsSyncedWrite(ctx) {
		return nil, nil
	}
	return s.beginCommentWrite(ctx, actor, comment, opUpdateComment, http.MethodPatch, &content, base)
}

// BeginCommentDelete deletes a comment Multica posted on GitHub. It returns
// nil for a comment that never reached GitHub. A comment GitHub no longer has
// counts as deleted.
func (s *Service) BeginCommentDelete(ctx context.Context, actor Actor, comment db.Comment) (*Write, error) {
	if s == nil || IsSyncedWrite(ctx) {
		return nil, nil
	}
	return s.beginCommentWrite(ctx, actor, comment, opDeleteComment, http.MethodDelete, nil, CommentBase{})
}

func (s *Service) beginCommentWrite(ctx context.Context, actor Actor, comment db.Comment, op, method string, content *string, base CommentBase) (*Write, error) {
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	if _, err := q.GetCommentExternal(ctx, db.GetCommentExternalParams{CommentID: comment.ID, WorkspaceID: comment.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if comment.AuthorType == externalAuthorType {
		return nil, &WriteError{Kind: WriteNotAllowed, msg: "comments synced from GitHub are read-only"}
	}
	// From here until Finish no other writer of this comment (another edit
	// or delete, the poller) runs, so the preconditions checked now still
	// hold when the local write lands.
	guard, err := s.lockTarget(ctx, commentLockKey(comment.ID))
	if err != nil {
		return nil, err
	}
	w, err := s.beginLockedCommentWrite(ctx, guard, actor, comment.ID, comment.WorkspaceID, op, method, content, base)
	if w == nil {
		guard.release()
	}
	return w, err
}

func (s *Service) beginLockedCommentWrite(ctx context.Context, guard *writeGuard, actor Actor, commentID, workspaceID pgtype.UUID,
	op, method string, content *string, base CommentBase) (*Write, error) {
	comment, err := guard.q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: commentID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if comment.DeletedAt.Valid {
		// The local write reports the deletion.
		return nil, nil
	}
	if base.ExpectedRevision != nil && comment.Revision != *base.ExpectedRevision {
		return nil, &StaleWriteError{Revision: true, Expected: *base.ExpectedRevision, Current: comment.Revision}
	}
	if content != nil {
		if base.Content != nil && comment.Content != *base.Content && comment.Content != *content {
			return nil, &StaleWriteError{}
		}
		if *content == comment.Content {
			return nil, nil
		}
	}
	cext, err := guard.q.GetCommentExternal(ctx, db.GetCommentExternalParams{CommentID: commentID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ext, err := guard.q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: comment.IssueID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if issueGone(ext) {
		return nil, nil
	}
	repo, err := s.repoForIssue(ctx, ext)
	if err != nil {
		return nil, err
	}
	intent, resp, err := s.send(ctx, writeRequest{
		op: op, actor: actor, repo: repo, method: method,
		path:    "/issues/comments/" + strconv.FormatInt(cext.ExternalID, 10),
		issueID: comment.IssueID, number: ext.ExternalNumber, commentID: comment.ID, githubID: cext.ExternalID,
		payload: func(opID pgtype.UUID) map[string]any {
			if content == nil {
				return nil
			}
			return map[string]any{"body": signedBody(*content, actor, opID)}
		},
	})
	if err != nil {
		return nil, err
	}
	w := &Write{svc: s, intent: intent, repo: repo, guard: guard}
	if content != nil {
		var gh githubComment
		if json.Unmarshal(resp.Body, &gh) == nil && gh.ID == cext.ExternalID {
			w.comment = &gh
		}
	}
	s.accept(ctx, &w.intent, cext.ExternalID, 0)
	return w, nil
}

// Finish records the outcome of the local write that followed GitHub's
// acceptance. localErr nil links the local copy to GitHub and settles the
// intent; otherwise the intent stays github_ok and the next poll heals the
// local copy from GitHub. A new issue uses InTx and Abandon instead.
func (w *Write) Finish(ctx context.Context, localErr error) {
	if w == nil {
		return
	}
	defer func() {
		w.done = true
		w.guard.release()
		w.guard = nil
	}()
	if localErr != nil {
		w.localFailed(ctx, localErr)
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := w.link(ctx); err != nil {
		w.svc.logger.Warn("github write: link local copy", "intent_id", w.intent.ID, "op", w.intent.Op, "error", err)
		w.svc.settleIntent(ctx, w.intent, intentGitHubOK, "link local copy: "+err.Error())
	}
}

// link records the GitHub side of the local copy and settles the intent, in
// one transaction.
func (w *Write) link(ctx context.Context) error {
	tx, err := w.svc.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	ws := w.intent.WorkspaceID
	switch w.intent.Op {
	case opUpdateIssue:
		if gh := w.issue; gh != nil {
			if _, err := q.UpsertIssueExternal(ctx, db.UpsertIssueExternalParams{
				IssueID: w.intent.IssueID, WorkspaceID: ws, Provider: issuekey.ProviderGitHub,
				ExternalRepoID: w.repo.id, ExternalRepo: w.repo.FullName(), ExternalNumber: gh.Number,
				ExternalNodeID: gh.NodeID, State: gh.State, StateReason: optionalText(gh.StateReason),
				ExternalUpdatedAt: pgtype.Timestamptz{Time: gh.UpdatedAt, Valid: true},
			}); err != nil {
				return err
			}
		}
	case opCreateComment, opUpdateComment:
		if gh := w.comment; gh != nil {
			var avatar pgtype.Text
			if gh.User.AvatarURL != "" {
				avatar = pgtype.Text{String: gh.User.AvatarURL, Valid: true}
			}
			if _, err := q.UpsertCommentExternal(ctx, db.UpsertCommentExternalParams{
				CommentID: w.intent.CommentID, WorkspaceID: ws, ExternalID: gh.ID,
				Login: SafeLogin(gh.User.Login), AvatarUrl: avatar,
				ExternalUpdatedAt: pgtype.Timestamptz{Time: gh.UpdatedAt, Valid: true},
			}); err != nil {
				return err
			}
		}
	case opDeleteComment:
		if err := q.DeleteCommentExternal(ctx, db.DeleteCommentExternalParams{CommentID: w.intent.CommentID, WorkspaceID: ws}); err != nil {
			return err
		}
	}
	if _, err := q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: w.intent.ID, WorkspaceID: ws, State: intentApplied,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return tx.Commit(ctx)
}
