// Added by the mouse9527/multica fork: syncing GitHub issues on request
// (server/internal/githubissues). Upstream has no such file.

package handler

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GitHubSyncResponse is the answer of a manual sync: what changed, and the
// issue's key.
type GitHubSyncResponse struct {
	githubissues.SyncResult
	Identifier string `json:"identifier"`
}

// SyncGitHubIssue serves POST /api/issues/{id}/github-sync, {id} being the
// issue's UUID, its key or "owner/repo#N": it reads the GitHub issue, its
// comments and linked pull requests now, within the request, and answers what
// changed. Mounted only while GitHub-native issues are on.
func (h *Handler) SyncGitHubIssue(w http.ResponseWriter, r *http.Request) {
	if h.GitHubIssues == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	result, err := h.GitHubIssues.SyncIssue(r.Context(), issue)
	if err != nil {
		writeSyncError(w, r, err, result)
		return
	}
	resp := issueToResponse(issue, h.getIssuePrefix(r.Context(), issue.WorkspaceID))
	h.fillIssueKeys(r.Context(), issue.WorkspaceID, &resp)
	writeJSON(w, http.StatusOK, GitHubSyncResponse{SyncResult: result, Identifier: resp.Identifier})
}

// writeSyncError answers a sync that failed or was refused, with what it had
// done by then: that stays stored, and syncing again picks up the rest.
func writeSyncError(w http.ResponseWriter, r *http.Request, err error, result githubissues.SyncResult) {
	body := map[string]any{
		"issue_id": result.IssueID, "issue_updated": result.IssueUpdated,
		"new_comments": result.NewComments, "comments_updated": result.CommentsUpdated,
		"pulls_updated": result.PullsUpdated, "requests_used": result.RequestsUsed,
	}
	var low *githubissues.QuotaLowError
	var limited *ghsnapshot.RateLimitError
	status := http.StatusBadGateway
	switch {
	case errors.As(err, &low):
		status = http.StatusTooManyRequests
		body["code"], body["error"] = "github_quota_low", "fewer GitHub requests are left this hour than the sync guard keeps; try again after the quota resets"
		body["remaining"], body["threshold"], body["reset_at"] = low.Remaining, low.Threshold, low.ResetAt
		setRetryAfter(w, body, low.RetryAfter)
	case errors.As(err, &limited):
		status = http.StatusServiceUnavailable
		body["code"], body["error"] = "github_rate_limited", "GitHub is rate limiting this server; try again later"
		setRetryAfter(w, body, limited.RetryAfter)
	case errors.Is(err, githubissues.ErrNotGitHubIssue):
		status = http.StatusUnprocessableEntity
		body["code"], body["error"] = "github_issue_local", "the issue is not a GitHub issue; there is nothing to sync"
	case errors.Is(err, githubissues.ErrRepoNotRegistered):
		status = http.StatusForbidden
		body["code"], body["error"] = "github_repo_not_registered", "the issue's repository is no longer registered on any project of this workspace"
	case errors.Is(err, githubissues.ErrRepoAmbiguous):
		status = http.StatusConflict
		body["code"], body["error"] = "github_repo_ambiguous", "the repository is registered under two names in this workspace"
	case errors.Is(err, githubissues.ErrRepoChanged):
		status = http.StatusConflict
		body["code"], body["error"] = "github_repo_changed", "the registered repository name now leads to another repository than the issue's"
	case errors.Is(err, githubissues.ErrTokenNotUsable):
		status = http.StatusServiceUnavailable
		body["code"], body["error"] = "github_token_unusable", "the GitHub token is not usable"
	case errors.Is(err, githubissues.ErrSyncIncomplete):
		body["code"], body["error"] = "github_sync_incomplete", "not all comments of the issue were read"
	default:
		slog.Warn("github issue sync failed", append(logger.RequestAttrs(r), "issue_id", result.IssueID, "error", err)...)
		body["code"], body["error"] = "github_sync_failed", "failed to sync the issue from GitHub"
	}
	writeJSON(w, status, body)
}

func setRetryAfter(w http.ResponseWriter, body map[string]any, wait time.Duration) {
	seconds := int(math.Ceil(max(wait, 0).Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	body["retry_after_seconds"] = seconds
}

// ProjectGitHubIssueResponse is a tracked GitHub issue of a project.
type ProjectGitHubIssueResponse struct {
	ID         string              `json:"id"`
	Identifier string              `json:"identifier"`
	GitHub     IssueGitHubResponse `json:"github"`
	State      string              `json:"state"`
}

// ListProjectGitHubIssues serves GET /api/projects/{id}/github-issues: the
// GitHub issues of the project a batch sync reads, by repository and number;
// ?open_only=true leaves out those closed on GitHub. It sends nothing to
// GitHub. Mounted only while GitHub-native issues are on.
func (h *Handler) ListProjectGitHubIssues(w http.ResponseWriter, r *http.Request) {
	if h.GitHubIssues == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	openOnly := r.URL.Query().Get("open_only") == "true"
	issues, err := h.GitHubIssues.ListProjectGitHubIssues(r.Context(), project.WorkspaceID, project.ID, openOnly)
	if err != nil {
		slog.Warn("list project github issues failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list the project's GitHub issues")
		return
	}
	prefix := h.getIssuePrefix(r.Context(), project.WorkspaceID)
	out := make([]ProjectGitHubIssueResponse, 0, len(issues))
	for _, issue := range issues {
		ext := db.IssueExternal{ExternalRepo: issue.Repo, ExternalNumber: issue.GitHubNumber}
		out = append(out, ProjectGitHubIssueResponse{
			ID:         uuidToString(issue.IssueID),
			Identifier: service.IssueIdentifier(prefix, issue.Number),
			GitHub:     *issueGitHubResponse(ext),
			State:      issue.State,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": out})
}
