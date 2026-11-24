// Added by the mouse9527/multica fork: Evolution comment events (M-229).

package evolution

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// An open issue page inserts a pushed comment into its timeline and sorts it
// by created_at; a payload without it crashed the page after an apply.
func TestCommentEventPayloadCarriesTimelineFields(t *testing.T) {
	at := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	c := db.Comment{
		ID:         pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		IssueID:    pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		AuthorType: "member",
		AuthorID:   pgtype.UUID{Bytes: [16]byte{3}, Valid: true},
		Content:    "Applied as version 1.",
		Type:       "comment",
		CreatedAt:  pgtype.Timestamptz{Time: at, Valid: true},
		UpdatedAt:  pgtype.Timestamptz{Time: at, Valid: true},
		Revision:   1,
	}
	got := commentEventPayload(c)
	if got["created_at"] != "2026-11-24T03:00:00Z" || got["updated_at"] != "2026-11-24T03:00:00Z" {
		t.Fatalf("created_at/updated_at = %v/%v, want the comment's time", got["created_at"], got["updated_at"])
	}
	if p, ok := got["parent_id"].(*string); !ok || p != nil {
		t.Fatalf("parent_id = %#v, want a nil *string (null)", got["parent_id"])
	}
	for _, key := range []string{"id", "issue_id", "author_type", "author_id", "content", "type", "revision", "reactions", "attachments", "resolved_at", "source_task_id"} {
		if _, ok := got[key]; !ok {
			t.Errorf("payload has no %q", key)
		}
	}
}
