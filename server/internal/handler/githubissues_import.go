// Added by the mouse9527/multica fork: importing GitHub issues on request
// (server/internal/githubissues). Upstream has no such file.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ImportGitHubIssueRequest names the GitHub issue to import: "owner/repo#N"
// or https://github.com/owner/repo/issues/N.
type ImportGitHubIssueRequest struct {
	Ref string `json:"ref"`
}

// ImportGitHubIssue serves POST /api/issues/import: it brings one GitHub
// issue of a repository registered in the workspace into Multica, after
// which it is followed like any GitHub issue. It answers 201 with the new
// issue, or 200 with the issue an earlier import already made. Mounted only
// while GitHub-native issues are on.
func (h *Handler) ImportGitHubIssue(w http.ResponseWriter, r *http.Request) {
	if h.GitHubIssues == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req ImportGitHubIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ref, err := githubissues.ParseIssueRef(req.Ref)
	if err != nil {
		writeImportError(w, r, err)
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	hidden, ok := h.requestHiddenProjects(w, r, uuidToString(wsUUID))
	if !ok {
		return
	}

	issueID, created, err := h.GitHubIssues.ImportIssue(r.Context(), wsUUID, ref, hidden.CanSeeProject)
	if errors.Is(err, githubissues.ErrImportIncomplete) {
		writeImportIncomplete(w, r, err, issueID)
		return
	}
	if err != nil {
		writeImportError(w, r, err)
		return
	}
	issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: wsUUID})
	if err != nil {
		slog.Warn("github issue import: load imported issue", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load imported issue")
		return
	}
	resp := issueToResponse(issue, h.getIssuePrefix(r.Context(), wsUUID))
	h.fillIssueKeys(r.Context(), wsUUID, &resp)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

// writeImportError answers an import that did not happen. Nothing was stored.
func writeImportError(w http.ResponseWriter, r *http.Request, err error) {
	var limited *ghsnapshot.RateLimitError
	switch {
	case errors.Is(err, githubissues.ErrInvalidIssueRef):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, githubissues.ErrPullRequest):
		writeImportErrorCode(w, http.StatusUnprocessableEntity, "github_pull_request", "the number is a pull request; only issues can be imported")
	case errors.Is(err, githubissues.ErrRepoNotRegistered):
		writeImportErrorCode(w, http.StatusForbidden, "github_repo_not_registered", "the repository is not registered on any project of this workspace")
	case errors.Is(err, githubissues.ErrRepoAmbiguous):
		writeImportErrorCode(w, http.StatusConflict, "github_repo_ambiguous", "the repository is registered under two names in this workspace")
	case errors.Is(err, githubissues.ErrGitHubIssueNotFound):
		writeImportErrorCode(w, http.StatusNotFound, "github_issue_not_found", "GitHub has no such issue")
	case errors.Is(err, githubissues.ErrSyncAccountUnavailable):
		writeImportErrorCode(w, http.StatusConflict, "github_sync_account_unavailable", "the GitHub sync account is not available in this workspace")
	case errors.Is(err, githubissues.ErrTokenNotUsable):
		writeImportErrorCode(w, http.StatusServiceUnavailable, "github_token_unusable", "the GitHub token is not usable")
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())))
		writeImportErrorCode(w, http.StatusServiceUnavailable, "github_rate_limited", "GitHub is rate limiting this server; try again later")
	case errors.Is(err, githubissues.ErrImportPaused):
		writeImportErrorCode(w, http.StatusServiceUnavailable, "github_rate_limited", "GitHub is rate limiting this server; try again later")
	default:
		slog.Warn("github issue import failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadGateway, "failed to import the issue from GitHub")
	}
}

// writeImportIncomplete answers an import that stored the issue but not all
// of its comments. The client imports again to finish it.
func writeImportIncomplete(w http.ResponseWriter, r *http.Request, err error, issueID pgtype.UUID) {
	status := http.StatusBadGateway
	var limited *ghsnapshot.RateLimitError
	if errors.As(err, &limited) {
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())))
		status = http.StatusServiceUnavailable
	} else {
		slog.Warn("github issue import incomplete", append(logger.RequestAttrs(r), "error", err)...)
	}
	writeJSON(w, status, map[string]any{
		"error":    "the issue is imported but not all of its comments were read; import it again to finish",
		"code":     "github_import_incomplete",
		"issue_id": uuidToString(issueID),
	})
}

func writeImportErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}
