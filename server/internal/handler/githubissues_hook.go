// Modified for the mouse9527/multica fork: GitHub-native issues
// (server/internal/githubissues). Upstream has no such hook.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ExternalCommentGuard identifies comments mirrored from GitHub comments
// written by people outside the workspace. GitHub is their only writer, so
// Multica refuses to edit or delete them.
type ExternalCommentGuard interface {
	ExternalCommentReadOnly(ctx context.Context, comment db.Comment) (bool, error)
}

// rejectExternalCommentWrite answers 403 for an external comment and reports
// whether it wrote a response. With the feature off ExternalComments is nil
// and every comment passes, as upstream.
func (h *Handler) rejectExternalCommentWrite(w http.ResponseWriter, r *http.Request, comment db.Comment) bool {
	if h.ExternalComments == nil {
		return false
	}
	readOnly, err := h.ExternalComments.ExternalCommentReadOnly(r.Context(), comment)
	if err != nil {
		slog.Warn("check external comment failed", "comment_id", uuidToString(comment.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to check comment source")
		return true
	}
	if readOnly {
		writeError(w, http.StatusForbidden, "comments synced from GitHub are read-only")
		return true
	}
	return false
}

// IssueViewRefresher refreshes an issue mirrored from GitHub when someone
// opens it. It returns at once; the refresh runs in the background.
type IssueViewRefresher interface {
	RefreshIssueOnView(ctx context.Context, issue db.Issue)
}

// refreshIssueOnView starts the background refresh of an opened issue. With
// the feature off IssueViews is nil and nothing happens, as upstream.
func (h *Handler) refreshIssueOnView(r *http.Request, issue db.Issue) {
	if h.IssueViews != nil {
		h.IssueViews.RefreshIssueOnView(r.Context(), issue)
	}
}

// GitHubStatusGuard refuses to set a GitHub-backed issue to a status its
// project cannot show on GitHub (githubissues.Service.CheckGitHubStatus).
type GitHubStatusGuard interface {
	CheckGitHubStatus(ctx context.Context, issue db.Issue, projectID pgtype.UUID, status string) error
}

// rejectUnmappedGitHubStatus answers 422 when the write sets a status GitHub
// cannot show, and reports whether it wrote a response. With the feature off
// GitHubStatuses is nil and every status passes, as upstream.
func (h *Handler) rejectUnmappedGitHubStatus(w http.ResponseWriter, r *http.Request, prev db.Issue, params db.UpdateIssueParams) bool {
	if h.GitHubStatuses == nil || !params.Status.Valid {
		return false
	}
	err := h.GitHubStatuses.CheckGitHubStatus(r.Context(), prev, params.ProjectID, params.Status.String)
	if err == nil {
		return false
	}
	var unmapped *githubissues.UnmappedStatusError
	if errors.As(err, &unmapped) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":            unmapped.Error(),
			"code":             "github_status_unmapped",
			"allowed_statuses": unmapped.Mappable,
		})
		return true
	}
	slog.Warn("check github status mapping failed", "issue_id", uuidToString(prev.ID), "error", err)
	writeError(w, http.StatusInternalServerError, "failed to check GitHub status mapping")
	return true
}

// gitHubStatusRejected is rejectUnmappedGitHubStatus for a batch, which skips
// a refused issue like its other per-issue guards.
func (h *Handler) gitHubStatusRejected(ctx context.Context, prev db.Issue, params db.UpdateIssueParams) bool {
	if h.GitHubStatuses == nil || !params.Status.Valid {
		return false
	}
	return h.GitHubStatuses.CheckGitHubStatus(ctx, prev, params.ProjectID, params.Status.String) != nil
}

// WriteGitHubStatus moves an issue to the status its GitHub state calls for
// by running UpdateIssue as the member userID, so a change made on GitHub
// takes the same path as one made in Multica: workflow checks, handoffs,
// runs and events. A refusal (4xx other than a write race) comes back as
// *githubissues.StatusWriteError; anything else is worth retrying.
func (h *Handler) WriteGitHubStatus(ctx context.Context, userID pgtype.UUID, issue db.Issue, status string) error {
	body, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return err
	}
	id := uuidToString(issue.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "/api/issues/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", uuidToString(userID))
	req.Header.Set("X-Workspace-ID", uuidToString(issue.WorkspaceID))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))

	res := &capturedResponse{header: http.Header{}, code: http.StatusOK}
	h.UpdateIssue(res, req)
	if res.code < http.StatusBadRequest {
		return nil
	}
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(res.body.Bytes(), &payload)
	if res.code >= http.StatusInternalServerError || res.code == http.StatusConflict {
		return fmt.Errorf("update issue status: %d %s", res.code, payload.Error)
	}
	return &githubissues.StatusWriteError{Code: res.code, Message: payload.Error}
}

// capturedResponse records the response of an in-process handler call.
type capturedResponse struct {
	header      http.Header
	code        int
	wroteHeader bool
	body        bytes.Buffer
}

func (c *capturedResponse) Header() http.Header { return c.header }

func (c *capturedResponse) WriteHeader(code int) {
	if !c.wroteHeader {
		c.code, c.wroteHeader = code, true
	}
}

func (c *capturedResponse) Write(p []byte) (int, error) {
	c.wroteHeader = true
	return c.body.Write(p)
}

// GitHub issues are written through to GitHub (docs/github-issues.md, "双写"):
// each hook below sends the GitHub part of a write before the handler writes
// the local copy, and reports the local outcome afterwards. With the feature
// off GitHubIssues is nil, every hook returns nil, and the handlers behave as
// upstream.

// gitHubActor is who the request writes as, with an agent's name for the
// signature line on GitHub.
func (h *Handler) gitHubActor(r *http.Request, workspaceID pgtype.UUID) githubissues.Actor {
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(workspaceID))
	actor := githubissues.Actor{Type: actorType, ID: parseUUIDOrZero(actorID)}
	if actorType == "agent" {
		if agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
			ID: actor.ID, WorkspaceID: workspaceID,
		}); err == nil {
			actor.Name = agent.Name
		}
	}
	return actor
}

// beginGitHubIssueUpdate sends the title, description and status of an issue
// update to GitHub. params are the update as it will be written locally;
// titleBase is the request's title_base. The preconditions (expected
// revision, title base) are checked under the issue's lock before anything is
// sent. Once GitHub has accepted, the expected revision has been met at that
// point and is cleared from params: a later write to a field GitHub does not
// hold must not turn an accepted change into a local 409.
func (h *Handler) beginGitHubIssueUpdate(r *http.Request, prev db.Issue, params *db.UpdateIssueParams, titleBase *string) (*githubissues.Write, error) {
	if h.GitHubIssues == nil {
		return nil, nil
	}
	change := githubissues.IssueChange{ProjectID: params.ProjectID, TitleBase: titleBase}
	if params.Title.Valid {
		change.Title = &params.Title.String
	}
	if params.Description.Valid {
		change.Description = &params.Description.String
	}
	if params.Status.Valid {
		change.Status = &params.Status.String
	}
	if params.ExpectedRevision.Valid {
		change.ExpectedRevision = &params.ExpectedRevision.Int64
	}
	write, err := h.GitHubIssues.BeginIssueUpdate(r.Context(), h.gitHubActor(r, prev.WorkspaceID), prev, change)
	if write != nil {
		params.ExpectedRevision = pgtype.Int8{}
	}
	return write, err
}

// beginGitHubIssueCreate decides where the new issue lives from the request's
// repo and provider and the issue's project (githubissues.ResolveCreateRepo),
// and prepares opening it on GitHub when that is a repository; the request
// itself goes out from InTx, after the local checks. projectID is the project
// the issue lands in, the parent's for a sub-issue without one. With the
// feature off both fields are ignored, as upstream ignores fields it does not
// know.
func (h *Handler) beginGitHubIssueCreate(w http.ResponseWriter, r *http.Request, workspaceID, projectID pgtype.UUID, creatorType string, req CreateIssueRequest) (*githubissues.Write, bool) {
	if h.GitHubIssues == nil {
		return nil, true
	}
	choice := githubissues.RepoChoice{ByAgent: creatorType == "agent"}
	if req.Repo != nil {
		choice.Repo = *req.Repo
	}
	if req.Provider != nil {
		choice.Provider = *req.Provider
	}
	repo, err := h.GitHubIssues.ResolveCreateRepo(r.Context(), workspaceID, projectID, choice)
	var refused *githubissues.RepoChoiceError
	if errors.As(err, &refused) {
		status, body := refused.HTTPResponse()
		writeJSON(w, status, body)
		return nil, false
	}
	if err != nil {
		slog.Warn("choose github repository failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to choose the GitHub repository")
		return nil, false
	}
	if repo == "" {
		return nil, true
	}
	write, err := h.GitHubIssues.BeginIssueCreate(r.Context(), workspaceID, h.gitHubActor(r, workspaceID), githubissues.NewIssue{Repo: repo})
	if err != nil {
		writeGitHubWriteError(w, r, err, "issue", pgtype.UUID{})
		return nil, false
	}
	return write, true
}

// writeGitHubCreateFailure answers a create that GitHub refused or did not
// confirm inside the create transaction, and reports whether it did. The
// create was rolled back.
func writeGitHubCreateFailure(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil || !githubissues.GitHubCreateError(err) {
		return false
	}
	writeGitHubWriteError(w, r, err, "issue", pgtype.UUID{})
	return true
}

// gitHubCreateInTx is the IssueCreateOpts.InTx of a create opened on GitHub.
func gitHubCreateInTx(write *githubissues.Write) func(context.Context, *db.Queries, db.Issue) error {
	if write == nil {
		return nil
	}
	return write.InTx
}

// beginGitHubCommentCreate posts a new comment of a GitHub issue to GitHub.
func (h *Handler) beginGitHubCommentCreate(w http.ResponseWriter, r *http.Request, issue db.Issue, content string) (*githubissues.Write, bool) {
	if h.GitHubIssues == nil {
		return nil, true
	}
	write, err := h.GitHubIssues.BeginCommentCreate(r.Context(), h.gitHubActor(r, issue.WorkspaceID), issue, content)
	if err != nil {
		writeGitHubWriteError(w, r, err, "comment", pgtype.UUID{})
		return nil, false
	}
	return write, true
}

// beginGitHubCommentChange edits (content set) or deletes (content nil) a
// comment that Multica posted on GitHub. contentBase and expectedRevision are
// the edit's preconditions, checked under the comment's lock before anything
// is sent.
func (h *Handler) beginGitHubCommentChange(w http.ResponseWriter, r *http.Request, comment db.Comment, content, contentBase *string, expectedRevision *int64) (*githubissues.Write, bool) {
	if h.GitHubIssues == nil {
		return nil, true
	}
	actor := h.gitHubActor(r, comment.WorkspaceID)
	var write *githubissues.Write
	var err error
	if content != nil {
		write, err = h.GitHubIssues.BeginCommentUpdate(r.Context(), actor, comment, *content, githubissues.CommentBase{
			Content: contentBase, ExpectedRevision: expectedRevision,
		})
	} else {
		write, err = h.GitHubIssues.BeginCommentDelete(r.Context(), actor, comment)
	}
	if err != nil {
		writeGitHubWriteError(w, r, err, "comment", comment.ID)
		return nil, false
	}
	return write, true
}

// writeGitHubWriteError answers a write GitHub did not take, or a stale one
// refused before it was sent, with the same 409 the local write would give.
// Nothing was written locally.
func writeGitHubWriteError(w http.ResponseWriter, r *http.Request, err error, resourceType string, resourceID pgtype.UUID) {
	var stale *githubissues.StaleWriteError
	if errors.As(err, &stale) {
		if stale.Revision {
			writeRevisionConflict(w, resourceType, resourceID, stale.Expected, stale.Current)
		} else {
			writeEditConflict(w, resourceType, resourceID)
		}
		return
	}
	var unmapped *githubissues.UnmappedStatusError
	if errors.As(err, &unmapped) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": unmapped.Error(), "code": "github_status_unmapped", "allowed_statuses": unmapped.Mappable,
		})
		return
	}
	var writeErr *githubissues.WriteError
	if !errors.As(err, &writeErr) {
		slog.Warn("github write failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to write to GitHub")
		return
	}
	status, body, retryAfter := writeErr.HTTPResponse()
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	writeJSON(w, status, body)
}

// DeleteGitHubComment removes the local copy of a comment GitHub no longer
// has, by running DeleteComment as the member userID, so tasks, events and
// thread tombstones are handled as for any delete. The context is marked as a
// synced write, so nothing is sent to GitHub. A comment already gone counts as
// deleted.
func (h *Handler) DeleteGitHubComment(ctx context.Context, userID pgtype.UUID, comment db.Comment) error {
	ctx = githubissues.WithSyncedWrite(ctx)
	id := uuidToString(comment.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "/api/comments/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-ID", uuidToString(userID))
	req.Header.Set("X-Workspace-ID", uuidToString(comment.WorkspaceID))
	route := chi.NewRouteContext()
	route.URLParams.Add("commentId", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))

	res := &capturedResponse{header: http.Header{}, code: http.StatusOK}
	h.DeleteComment(res, req)
	if res.code < http.StatusBadRequest || res.code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete comment: %d %s", res.code, strings.TrimSpace(res.body.String()))
}
