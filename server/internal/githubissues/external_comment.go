package githubissues

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// External comments are stored with author_type 'system' and the zero author
// id, the identity platform notices already use (comment.author_type admits
// it without a constraint change). comment_external carries the GitHub
// comment id, login and avatar.
const externalAuthorType = "system"

var (
	// ErrNoDatabase means the Service was built without a database.
	ErrNoDatabase = errors.New("githubissues: no database configured")
	// ErrIssueNotFound means the target issue is not in the workspace.
	ErrIssueNotFound = errors.New("githubissues: issue not found in workspace")
	// ErrNotExternalComment means the GitHub comment is mirrored by a comment
	// with a Multica author (one written from Multica), which this path never
	// rewrites.
	ErrNotExternalComment = errors.New("githubissues: comment has a Multica author")
	// ErrCommentMoved means the GitHub comment is already mirrored on another
	// issue.
	ErrCommentMoved = errors.New("githubissues: github comment is mirrored on another issue")
)

// ExternalComment is a GitHub issue comment written by someone outside the
// workspace, as the poller read it.
type ExternalComment struct {
	WorkspaceID pgtype.UUID
	IssueID     pgtype.UUID
	// ExternalID is the GitHub comment id.
	ExternalID int64
	Login      string
	AvatarURL  string
	// Body is the comment text exactly as GitHub returned it.
	Body string
	// UpdatedAt is GitHub's updated_at; an older snapshot never overwrites a
	// newer one.
	UpdatedAt time.Time
}

// CommentSyncResult reports what SyncExternalComment stored.
type CommentSyncResult struct {
	Comment db.Comment
	Created bool
	// Changed is true when an existing comment's text was rewritten.
	Changed bool
}

// SyncExternalComment stores a GitHub comment written by someone outside the
// workspace, or refreshes the comment that already mirrors it.
//
// The comment never goes through the comment trigger pipeline: it starts no
// agent run (assignee, @mention or thread reply), creates no inbox item and
// adds no subscriber, and the wakeup receipts its insert or edit produced are
// discarded in the same transaction. The comment:created / comment:updated
// event is still published so timelines update live.
func (s *Service) SyncExternalComment(ctx context.Context, in ExternalComment) (CommentSyncResult, error) {
	if !in.WorkspaceID.Valid || !in.IssueID.Valid || in.ExternalID <= 0 || in.UpdatedAt.IsZero() {
		return CommentSyncResult{}, errors.New("githubissues: incomplete external comment")
	}
	q, err := s.queries()
	if err != nil {
		return CommentSyncResult{}, err
	}
	issue, err := q.GetIssue(ctx, in.IssueID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && issue.WorkspaceID != in.WorkspaceID) {
		return CommentSyncResult{}, ErrIssueNotFound
	}
	if err != nil {
		return CommentSyncResult{}, err
	}
	// Two syncs racing on a new GitHub comment both insert; the unique
	// (workspace_id, external_id) index rejects the loser, whose retry finds
	// the winner's row.
	var result CommentSyncResult
	var issueRevision int64
	for attempt := 0; ; attempt++ {
		result, issueRevision, err = s.syncExternalComment(ctx, in)
		var pgErr *pgconn.PgError
		if attempt == 0 && errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		break
	}
	if err != nil {
		return CommentSyncResult{}, err
	}
	s.publishExternalComment(issue, result, issueRevision)
	return result, nil
}

func (s *Service) syncExternalComment(ctx context.Context, in ExternalComment) (CommentSyncResult, int64, error) {
	login := SafeLogin(in.Login)
	content := ExternalCommentContent(login, in.Body)
	updatedAt := pgtype.Timestamptz{Time: in.UpdatedAt, Valid: true}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CommentSyncResult{}, 0, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)

	var result CommentSyncResult
	var issueRevision int64
	existing, err := q.LockCommentExternalByExternalID(ctx, db.LockCommentExternalByExternalIDParams{
		WorkspaceID: in.WorkspaceID, ExternalID: in.ExternalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		created, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID:          dbid.NewV7(),
			IssueID:     in.IssueID,
			WorkspaceID: in.WorkspaceID,
			AuthorType:  externalAuthorType,
			AuthorID:    pgtype.UUID{Valid: true},
			Content:     content,
			Type:        "comment",
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return CommentSyncResult{}, 0, ErrIssueNotFound
		}
		if err != nil {
			return CommentSyncResult{}, 0, err
		}
		result = CommentSyncResult{Comment: created.Comment(), Created: true}
		issueRevision = created.IssueRevision
	case err != nil:
		return CommentSyncResult{}, 0, err
	default:
		comment, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
			ID: existing.CommentID, WorkspaceID: in.WorkspaceID,
		})
		if err != nil {
			return CommentSyncResult{}, 0, err
		}
		if comment.AuthorType != externalAuthorType {
			return CommentSyncResult{}, 0, ErrNotExternalComment
		}
		if comment.IssueID != in.IssueID {
			return CommentSyncResult{}, 0, ErrCommentMoved
		}
		result.Comment = comment
		fresh := !in.UpdatedAt.Before(existing.ExternalUpdatedAt.Time)
		if fresh && !comment.DeletedAt.Valid && comment.Content != content {
			updated, err := q.UpdateComment(ctx, db.UpdateCommentParams{ID: comment.ID, Content: content})
			if err != nil {
				return CommentSyncResult{}, 0, err
			}
			result.Comment = updated.Comment()
			result.Changed = true
			issueRevision = updated.IssueRevision
		}
	}

	var avatar pgtype.Text
	if in.AvatarURL != "" {
		avatar = pgtype.Text{String: in.AvatarURL, Valid: true}
	}
	if _, err := q.UpsertCommentExternal(ctx, db.UpsertCommentExternalParams{
		CommentID:         result.Comment.ID,
		WorkspaceID:       in.WorkspaceID,
		ExternalID:        in.ExternalID,
		Login:             login,
		AvatarUrl:         avatar,
		ExternalUpdatedAt: updatedAt,
	}); err != nil {
		return CommentSyncResult{}, 0, err
	}
	if result.Created || result.Changed {
		if _, err := q.DiscardExternalCommentWakeupReceipts(ctx, db.DiscardExternalCommentWakeupReceiptsParams{
			IssueID: in.IssueID, CommentID: util.UUIDToString(result.Comment.ID),
		}); err != nil {
			return CommentSyncResult{}, 0, fmt.Errorf("discard wakeup receipts: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CommentSyncResult{}, 0, err
	}
	return result, issueRevision, nil
}

// publishExternalComment broadcasts the change for live timelines. The
// notification and subscriber listeners skip system-authored comments.
func (s *Service) publishExternalComment(issue db.Issue, result CommentSyncResult, issueRevision int64) {
	if s.bus == nil || (!result.Created && !result.Changed) {
		return
	}
	c := result.Comment
	payload := map[string]any{
		"comment": map[string]any{
			"id":             util.UUIDToString(c.ID),
			"issue_id":       util.UUIDToString(c.IssueID),
			"author_type":    c.AuthorType,
			"author_id":      util.UUIDToString(c.AuthorID),
			"content":        c.Content,
			"type":           c.Type,
			"parent_id":      util.UUIDToPtr(c.ParentID),
			"source_task_id": util.UUIDToPtr(c.SourceTaskID),
			"created_at":     util.TimestampToString(c.CreatedAt),
			"updated_at":     util.TimestampToString(c.UpdatedAt),
			"revision":       c.Revision,
		},
	}
	if issueRevision > 0 {
		payload["issue_revision"] = issueRevision
	}
	eventType := protocol.EventCommentUpdated
	if result.Created {
		eventType = protocol.EventCommentCreated
		payload["issue_title"] = issue.Title
		payload["issue_status"] = issue.Status
	}
	s.bus.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   externalAuthorType,
		Payload:     payload,
	})
}

// ExternalCommentReadOnly reports whether a comment mirrors a GitHub comment
// written by someone outside the workspace. Such comments are read-only in
// Multica: GitHub is their only writer.
func (s *Service) ExternalCommentReadOnly(ctx context.Context, comment db.Comment) (bool, error) {
	if comment.AuthorType != externalAuthorType {
		return false, nil
	}
	q, err := s.queries()
	if err != nil {
		return false, err
	}
	_, err = q.GetCommentExternal(ctx, db.GetCommentExternalParams{
		CommentID: comment.ID, WorkspaceID: comment.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
