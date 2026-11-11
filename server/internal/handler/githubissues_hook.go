// Modified for the mouse9527/multica fork: GitHub-native issues
// (server/internal/githubissues). Upstream has no such hook.

package handler

import (
	"context"
	"log/slog"
	"net/http"

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
