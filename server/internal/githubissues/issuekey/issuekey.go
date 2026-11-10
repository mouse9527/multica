// Package issuekey renders and parses the identifier of GitHub-backed issues
// for the mouse9527/multica fork. It is fork-only and not part of upstream
// multica-ai/multica.
//
// An issue is GitHub-backed exactly when it has an issue_external row; its
// identifier is then "owner/repo#N" instead of the workspace's "PREFIX-N".
// The package depends only on the generated queries so that service, handler
// and githubissues can all use it without an import cycle.
package issuekey

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Provider values carried on issue responses.
const (
	ProviderLocal  = "local"
	ProviderGitHub = "github"
)

// GitHub renders the identifier of a GitHub-backed issue: "owner/repo#N".
func GitHub(ext db.IssueExternal) string {
	return ext.ExternalRepo + "#" + strconv.Itoa(int(ext.ExternalNumber))
}

// GitHub owner and repository names: owners are alphanumerics and hyphens,
// repositories also allow dots and underscores.
var githubKey = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100})#([0-9]{1,10})$`)

// ParseGitHub splits an "owner/repo#N" identifier. ok is false for anything
// else, including local "PREFIX-N" identifiers and numbers outside int32.
func ParseGitHub(key string) (repo string, number int32, ok bool) {
	m := githubKey.FindStringSubmatch(key)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return m[1], int32(n), true
}

// Querier is the part of *db.Queries this package reads.
type Querier interface {
	GetIssueExternal(ctx context.Context, arg db.GetIssueExternalParams) (db.IssueExternal, error)
	ListIssueExternalsByIssueIDs(ctx context.Context, arg db.ListIssueExternalsByIssueIDsParams) ([]db.IssueExternal, error)
}

// Externals holds the GitHub identity of a set of issues, keyed by issue id.
// An issue missing from it is local. The zero value is an empty set.
type Externals map[pgtype.UUID]db.IssueExternal

// Load reads the GitHub identity of issueIDs in one query; no ids, no query.
// A failed read is logged and leaves every issue rendered as local, the way
// list endpoints degrade when labels fail to load.
func Load(ctx context.Context, q Querier, workspaceID pgtype.UUID, issueIDs []pgtype.UUID) Externals {
	if len(issueIDs) == 0 {
		return Externals{}
	}
	rows, err := q.ListIssueExternalsByIssueIDs(ctx, db.ListIssueExternalsByIssueIDsParams{
		WorkspaceID: workspaceID,
		IssueIds:    issueIDs,
	})
	if err != nil {
		slog.Warn("issuekey: load issue externals failed", "error", err, "count", len(issueIDs))
		return Externals{}
	}
	out := make(Externals, len(rows))
	for _, row := range rows {
		out[row.IssueID] = row
	}
	return out
}

// LoadOne is Load for a single issue.
func LoadOne(ctx context.Context, q Querier, workspaceID, issueID pgtype.UUID) Externals {
	row, err := q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issueID, WorkspaceID: workspaceID})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("issuekey: load issue external failed", "error", err)
		}
		return Externals{}
	}
	return Externals{issueID: row}
}

// Key returns the GitHub identifier of issueID, or local when it is not
// GitHub-backed.
func (e Externals) Key(issueID pgtype.UUID, local string) string {
	if ext, ok := e[issueID]; ok {
		return GitHub(ext)
	}
	return local
}

// Provider returns ProviderGitHub or ProviderLocal for issueID.
func (e Externals) Provider(issueID pgtype.UUID) string {
	if ext, ok := e[issueID]; ok {
		return ext.Provider
	}
	return ProviderLocal
}
