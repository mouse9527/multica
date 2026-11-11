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

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues"
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
