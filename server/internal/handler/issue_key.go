// Added by the mouse9527/multica fork: identifiers of GitHub-native issues.
// Upstream has no such file.

package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IssueGitHubResponse is where a GitHub-backed issue lives on GitHub. The
// issue's identifier stays its workspace key "PREFIX-N"; this is carried
// beside it.
type IssueGitHubResponse struct {
	Repo   string `json:"repo"`
	Number int32  `json:"number"`
	URL    string `json:"url"`
}

func issueGitHubResponse(ext db.IssueExternal) *IssueGitHubResponse {
	return &IssueGitHubResponse{Repo: ext.ExternalRepo, Number: ext.ExternalNumber, URL: issuekey.URL(ext)}
}

// fillIssueKeys sets provider and the github field on responses built by the
// pure builders (which render every issue as local), from ONE read of
// issue_external for the whole page. Identifiers are left as they are.
func (h *Handler) fillIssueKeys(ctx context.Context, wsID pgtype.UUID, resps ...*IssueResponse) {
	ids := make([]pgtype.UUID, 0, len(resps))
	for _, resp := range resps {
		if resp == nil {
			continue
		}
		if id, err := util.ParseUUID(resp.ID); err == nil {
			ids = append(ids, id)
		}
	}
	externals := issuekey.Load(ctx, h.Queries, wsID, ids)
	if len(externals) == 0 {
		return
	}
	for _, resp := range resps {
		if resp == nil {
			continue
		}
		if ext, ok := externals[parseUUIDOrZero(resp.ID)]; ok {
			resp.Provider = ext.Provider
			resp.GitHub = issueGitHubResponse(ext)
		}
	}
}

// fillIssueKeySlice is fillIssueKeys for a slice of responses.
func (h *Handler) fillIssueKeySlice(ctx context.Context, wsID pgtype.UUID, resps []IssueResponse) {
	ptrs := make([]*IssueResponse, len(resps))
	for i := range resps {
		ptrs[i] = &resps[i]
	}
	h.fillIssueKeys(ctx, wsID, ptrs...)
}

// issueKey renders one issue's identifier: its workspace key.
func (h *Handler) issueKey(ctx context.Context, issue db.Issue) string {
	return service.IssueIdentifier(h.getIssuePrefix(ctx, issue.WorkspaceID), issue.Number)
}

// loadIssueKeys is what loops rendering identifiers outside IssueResponse
// pass to service.IssueKey. Identifiers no longer depend on issue_external,
// so it reads nothing.
func (h *Handler) loadIssueKeys(context.Context, pgtype.UUID, []pgtype.UUID) issuekey.Externals {
	return nil
}

// resolveIssueByGitHubKey resolves an "owner/repo#N" identifier in the
// workspace. The key may arrive percent-encoded ("owner%2Frepo%23N"): the
// router matches on the escaped path, so the URL param keeps the escapes.
func (h *Handler) resolveIssueByGitHubKey(ctx context.Context, id, workspaceID string) (db.Issue, bool) {
	if !issuekey.Enabled() || (!strings.Contains(id, "#") && !strings.Contains(id, "%")) {
		return db.Issue{}, false
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return db.Issue{}, false
	}
	if strings.Contains(id, "%") {
		unescaped, err := url.PathUnescape(id)
		if err != nil {
			return db.Issue{}, false
		}
		id = unescaped
	}
	repo, number, ok := issuekey.ParseGitHub(id)
	if !ok {
		return db.Issue{}, false
	}
	ext, err := h.Queries.GetIssueExternalByRepoName(ctx, db.GetIssueExternalByRepoNameParams{
		WorkspaceID:    wsUUID,
		Provider:       issuekey.ProviderGitHub,
		ExternalRepo:   repo,
		ExternalNumber: number,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("resolve github issue key failed", "error", err)
		}
		return db.Issue{}, false
	}
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: ext.IssueID, WorkspaceID: wsUUID})
	if err != nil {
		return db.Issue{}, false
	}
	return issue, true
}

func parseUUIDOrZero(s string) pgtype.UUID {
	id, err := util.ParseUUID(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}
