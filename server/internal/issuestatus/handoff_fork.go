// Modified for the mouse9527/multica fork: handoff reply wake (M-89).

package issuestatus

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// IsHandoff reports whether status hands the issue back to a person: done and
// closed always, started except the platform's own in_progress key.
//
// It is the same rule as issueStatusIsHandoff in cmd/server (package main, so
// not importable); TestForkIsHandoffMatchesUpstream there keeps the two
// aligned. Errors come only from resolving a custom status.
func IsHandoff(ctx context.Context, q Querier, workspaceID pgtype.UUID, status string) (bool, error) {
	category, err := CategoryWithError(ctx, q, workspaceID, status)
	if err != nil {
		return false, err
	}
	switch category {
	case CategoryDone, CategoryClosed:
		return true, nil
	case CategoryStarted:
		return status != InProgress, nil
	default:
		return false, nil
	}
}
