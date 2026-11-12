// Added by the mouse9527/multica fork: identifiers of GitHub-native issues.
// Upstream has no such file.

package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
)

// IssueKey renders an issue's identifier. A GitHub-backed issue keeps its
// workspace key "PREFIX-N" like any other, so this is IssueIdentifier; it
// stays as the seam the fork's call sites in upstream files go through until
// they are removed.
func IssueKey(_ issuekey.Externals, _ pgtype.UUID, issuePrefix string, number int32) string {
	return IssueIdentifier(issuePrefix, number)
}

// LookupIssueIdentifier is IssueKey for one issue. It reads nothing.
func LookupIssueIdentifier(_ context.Context, _ issuekey.Querier, _, _ pgtype.UUID, issuePrefix string, number int32) string {
	return IssueIdentifier(issuePrefix, number)
}

// applyIssueKeyToMap sets provider, and the github field of a GitHub-backed
// issue, on an IssueToMap payload; the identifier stays the workspace key.
// Queriers that cannot read issue_external (test fakes) leave the payload
// local.
func applyIssueKeyToMap(ctx context.Context, q any, m map[string]any, workspaceID, issueID pgtype.UUID) {
	kq, ok := q.(issuekey.Querier)
	if !ok {
		return
	}
	externals := issuekey.LoadOne(ctx, kq, workspaceID, issueID)
	m["provider"] = externals.Provider(issueID)
	if ext, ok := externals[issueID]; ok {
		m["github"] = map[string]any{
			"repo":   ext.ExternalRepo,
			"number": ext.ExternalNumber,
			"url":    issuekey.URL(ext),
		}
	}
}
