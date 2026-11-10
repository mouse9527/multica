package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// githubComment is the part of a GitHub issue comment the poller reads.
type githubComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
	// IssueURL ends in the number of the issue or pull request.
	IssueURL string `json:"issue_url"`
	User     struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
}

// issueNumber reads the issue number at the end of issue_url.
func (c githubComment) issueNumber() (int32, bool) {
	i := strings.LastIndex(c.IssueURL, "/")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(c.IssueURL[i+1:], 10, 32)
	return int32(n), err == nil && n > 0
}

// pollComments reads the comments stream of a repository from cursor. The
// stream has its own cursor instead of relying on a comment to move its
// issue's updated_at, which GitHub does not document.
//
// A comment whose issue has no local mirror is skipped: the stream also lists
// comments on pull requests, and the issues stream, read first, has already
// imported every issue it could.
func (s *Service) pollComments(ctx context.Context, repo *Repo, cursor pgtype.Timestamptz) (synced int, err error) {
	q := db.New(s.db)
	_, err = s.readStream(ctx, repo, stream{path: "/issues/comments", comments: true}, cursor, func(items []json.RawMessage) (int, error) {
		for i, raw := range items {
			var c githubComment
			if err := json.Unmarshal(raw, &c); err != nil || c.ID <= 0 {
				return i, errors.New("github GET /issues/comments: malformed comment")
			}
			number, ok := c.issueNumber()
			if !ok {
				return i, errors.New("github GET /issues/comments: comment without issue number")
			}
			mirror, err := q.GetIssueExternalByNumber(ctx, db.GetIssueExternalByNumberParams{
				WorkspaceID: repo.workspaceID, Provider: issuekey.ProviderGitHub,
				ExternalRepoID: repo.id, ExternalNumber: number,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return i, err
			}
			res, err := s.SyncExternalComment(ctx, ExternalComment{
				WorkspaceID: repo.workspaceID, IssueID: mirror.IssueID, ExternalID: c.ID,
				Login: c.User.Login, AvatarURL: c.User.AvatarURL, Body: c.Body, UpdatedAt: c.UpdatedAt,
			})
			switch {
			case err == nil:
				if res.Created || res.Changed {
					synced++
				}
			case errors.Is(err, ErrNotExternalComment):
				// Written from Multica; its local row is already the mirror.
			case errors.Is(err, ErrIssueNotFound), errors.Is(err, ErrCommentMoved):
				s.logger.Warn("github issues poll: comment skipped", "repo", repo,
					"issue_number", number, "comment_id", c.ID, "error", err)
			default:
				return i, fmt.Errorf("sync comment %d on %s#%d: %w", c.ID, repo.FullName(), number, err)
			}
		}
		return len(items), nil
	})
	return synced, err
}
