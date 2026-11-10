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

// fillIssueKeys sets identifier and provider on responses built by the pure
// builders (which render every issue as local), and the identifier of their
// duplicate_of originals, from ONE read of issue_external for the whole page.
//
// Run it after the status filler: fillDuplicateOf derives the original's local
// identifier from the response's own, so it must still be the local one.
func (h *Handler) fillIssueKeys(ctx context.Context, wsID pgtype.UUID, resps ...*IssueResponse) {
	ids := make([]pgtype.UUID, 0, len(resps))
	for _, resp := range resps {
		if resp == nil {
			continue
		}
		if id, err := util.ParseUUID(resp.ID); err == nil {
			ids = append(ids, id)
		}
		if resp.DuplicateOf != nil {
			if id, err := util.ParseUUID(resp.DuplicateOf.ID); err == nil {
				ids = append(ids, id)
			}
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
			resp.Identifier = issuekey.GitHub(ext)
			resp.Provider = ext.Provider
		}
		if resp.DuplicateOf != nil {
			if ext, ok := externals[parseUUIDOrZero(resp.DuplicateOf.ID)]; ok {
				resp.DuplicateOf.Identifier = issuekey.GitHub(ext)
			}
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

// issueKey renders one issue's identifier, reading its issue_external row.
func (h *Handler) issueKey(ctx context.Context, issue db.Issue) string {
	return service.LookupIssueIdentifier(ctx, h.Queries, issue.WorkspaceID, issue.ID, h.getIssuePrefix(ctx, issue.WorkspaceID), issue.Number)
}

// loadIssueKeys reads the GitHub identity of a set of issues in one query, for
// loops that render identifiers outside IssueResponse.
func (h *Handler) loadIssueKeys(ctx context.Context, wsID pgtype.UUID, ids []pgtype.UUID) issuekey.Externals {
	return issuekey.Load(ctx, h.Queries, wsID, ids)
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
