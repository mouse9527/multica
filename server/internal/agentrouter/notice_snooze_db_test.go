// Modified for the mouse9527/multica fork: M-131 (upstream sync). A Router
// notice is announced like every other inbox row, snooze included.

package agentrouter

import (
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestInboxNoticeOnSnoozedIssueIsAnnouncedAsSnoozed(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(func() { e.fx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1`, e.ws) })
	snoozed := e.issue("router notice snoozed")
	open := e.issue("router notice open")
	// The owner snoozed the first issue: one of its rows is out until tomorrow.
	e.fx.Exec(t, `
		INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, severity, issue_id, title, snoozed_until, snoozed_at)
		VALUES ($1, 'member', $2, 'status_changed', 'info', $3, 'earlier', now() + interval '1 day', now())`,
		e.ws, e.user, snoozed)

	var mu sync.Mutex
	got := map[string]string{} // inbox item id → event type
	bus := events.New()
	for _, typ := range []string{protocol.EventInboxNew, protocol.EventInboxSnoozed} {
		bus.Subscribe(typ, func(ev events.Event) {
			item, _ := ev.Payload.(map[string]any)["item"].(map[string]any)
			id, _ := item["id"].(string)
			mu.Lock()
			got[id] = ev.Type
			mu.Unlock()
		})
	}
	adapter := InboxAdapter{Queries: db.New(pool), Bus: bus}
	deliver := func(issue string) string {
		t.Helper()
		d := adapter.Deliver(e.ctx, Notice{
			ID: util.UUIDToString(dbid.NewV7()), WorkspaceID: e.ws, RecipientUserID: e.user,
			Title: "router notice", Refs: []string{issue}, Urgency: UrgencyHigh, SenderType: "agent", SenderID: &e.agentA,
		})
		if d.State != DeliverySent {
			t.Fatalf("delivery = %+v, want sent", d)
		}
		return d.Ref
	}

	onSnoozed := deliver(snoozed)
	onOpen := deliver(open)
	mu.Lock()
	defer mu.Unlock()
	if got[onSnoozed] != protocol.EventInboxSnoozed {
		t.Errorf("notice on a snoozed issue announced as %q, want %q (no banner, no push)", got[onSnoozed], protocol.EventInboxSnoozed)
	}
	if got[onOpen] != protocol.EventInboxNew {
		t.Errorf("notice on an open issue announced as %q, want %q", got[onOpen], protocol.EventInboxNew)
	}
	var stillSnoozed bool
	if err := pool.QueryRow(e.ctx, `SELECT snoozed_until > now() FROM inbox_item WHERE id = $1`, onSnoozed).Scan(&stillSnoozed); err != nil {
		t.Fatal(err)
	}
	if !stillSnoozed {
		t.Error("a Router notice ended the snooze; it should join it")
	}
}
