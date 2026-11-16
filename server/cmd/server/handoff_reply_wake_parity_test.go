// Modified for the mouse9527/multica fork: handoff reply wake (M-89).

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestForkIsHandoffMatchesUpstream keeps issuestatus.IsHandoff, which the
// fork's handoff reply wake uses from the handler package, answering exactly
// like the upstream issueStatusIsHandoff it copies.
func TestForkIsHandoffMatchesUpstream(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := util.MustParseUUID(testWorkspaceID)

	statuses := []string{
		issuestatus.Backlog, issuestatus.Todo, issuestatus.InProgress, issuestatus.InReview,
		issuestatus.Done, issuestatus.Blocked, issuestatus.Cancelled,
		"status_that_was_never_created",
	}
	for _, category := range []string{issuestatus.CategoryUnstarted, issuestatus.CategoryStarted, issuestatus.CategoryDone, issuestatus.CategoryClosed} {
		key := fmt.Sprintf("parity_%s_%d", category, time.Now().UnixNano()%1_000_000)
		entry, err := queries.CreateIssueStatusEntry(ctx, db.CreateIssueStatusEntryParams{
			WorkspaceID: workspaceID, Key: key, Name: key, Category: category, Color: "#123456",
		})
		if err != nil {
			t.Fatalf("create custom status in %q: %v", category, err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE id = $1`, entry.ID)
		})
		statuses = append(statuses, entry.Key)
	}

	for _, status := range statuses {
		want, wantErr := issueStatusIsHandoff(ctx, queries, workspaceID, status)
		got, gotErr := issuestatus.IsHandoff(ctx, queries, workspaceID, status)
		if got != want || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("%q: fork (%v, %v) != upstream (%v, %v)", status, got, gotErr, want, wantErr)
		}
	}
}
