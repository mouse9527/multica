// Added by the mouse9527/multica fork: identifiers of GitHub-native issues.
// Upstream has no such file.

package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
)

// IssueKey renders an issue's identifier: "owner/repo#N" when externals holds
// its GitHub identity, IssueIdentifier otherwise. Callers rendering a page of
// issues load externals once with issuekey.Load.
func IssueKey(externals issuekey.Externals, issueID pgtype.UUID, issuePrefix string, number int32) string {
	return externals.Key(issueID, IssueIdentifier(issuePrefix, number))
}

// LookupIssueIdentifier is IssueKey for one issue, reading its issue_external
// row.
func LookupIssueIdentifier(ctx context.Context, q issuekey.Querier, workspaceID, issueID pgtype.UUID, issuePrefix string, number int32) string {
	return IssueKey(issuekey.LoadOne(ctx, q, workspaceID, issueID), issueID, issuePrefix, number)
}

// applyIssueKeyToMap sets identifier and provider on an IssueToMap payload,
// and the identifier of its duplicate_of original, from one batch read.
// Queriers that cannot read issue_external (test fakes) leave the payload
// local.
func applyIssueKeyToMap(ctx context.Context, q any, m map[string]any, workspaceID, issueID pgtype.UUID, issuePrefix string, number int32, original *pgtype.UUID) {
	kq, ok := q.(issuekey.Querier)
	if !ok {
		return
	}
	ids := []pgtype.UUID{issueID}
	if original != nil {
		ids = append(ids, *original)
	}
	externals := issuekey.Load(ctx, kq, workspaceID, ids)
	m["identifier"] = IssueKey(externals, issueID, issuePrefix, number)
	m["provider"] = externals.Provider(issueID)
	if original == nil {
		return
	}
	if ref, ok := m["duplicate_of"].(map[string]any); ok {
		if ext, ok := externals[*original]; ok {
			ref["identifier"] = issuekey.GitHub(ext)
		}
	}
}
