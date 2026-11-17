// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).

package agentrouter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// shadowHold registers a HoldShadow for this test as the inbox interceptor.
func (e *env) shadowHold() *HoldShadow {
	e.t.Helper()
	h := NewHoldShadow(e.store)
	inbox.SetInterceptor(h, 5*time.Second)
	e.t.Cleanup(func() {
		inbox.SetInterceptor(nil, 0)
		e.fx.Exec(e.t, `DELETE FROM inbox_item WHERE workspace_id = $1`, e.ws)
	})
	return h
}

type inboxWrite struct {
	recipient, issue, typ, actorType, actorID string
}

// writeInbox writes one inbox item through CreateIfEligible and returns its
// id. The write must land: the hold is in shadow mode.
func (e *env) writeInbox(w inboxWrite) string {
	e.t.Helper()
	p := db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: util.MustParseUUID(e.ws), RecipientType: "member",
		RecipientID: util.MustParseUUID(w.recipient), Type: w.typ, Severity: "info", Title: "item " + w.typ,
		Details: []byte(`{"k":"v"}`),
	}
	if w.issue != "" {
		p.IssueID = util.MustParseUUID(w.issue)
	}
	if w.actorType != "" {
		p.ActorType.String, p.ActorType.Valid = w.actorType, true
		p.ActorID = util.MustParseUUID(w.actorID)
	}
	item, inserted, err := inbox.CreateIfEligible(e.ctx, db.New(pool), p)
	if err != nil || !inserted {
		e.t.Fatalf("inbox write: inserted=%v err=%v; shadow mode must not keep items out", inserted, err)
	}
	return util.UUIDToString(item.ID)
}

func (e *env) holds() []Hold {
	e.t.Helper()
	out, err := e.store.ListHolds(e.ctx, HoldFilter{WorkspaceID: e.ws, Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestShadowHoldRecordsWhatTheHoldWouldTake(t *testing.T) {
	e := newEnv(t)
	root := e.issue("router root")
	child := e.issue("router child", testutil.Cols{"parent_issue_id": root})
	outside := e.issue("outside")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)
	other := e.fx.User(t, "router other", fmt.Sprintf("router-other-%d@example.test", time.Now().UnixNano()))
	e.fx.Member(t, e.ws, other, "member")
	h := e.shadowHold()

	taken := e.writeInbox(inboxWrite{recipient: e.user, issue: child, typ: "mentioned", actorType: "agent", actorID: e.agentB})
	for name, w := range map[string]inboxWrite{
		"outside the scope":     {recipient: e.user, issue: outside, typ: "mentioned"},
		"for someone else":      {recipient: other, issue: child, typ: "mentioned"},
		"the Router's notice":   {recipient: e.user, issue: child, typ: InboxNoticeType},
		"a connected-app ask":   {recipient: e.user, issue: child, typ: "app_action_requested"},
		"the Router's own work": {recipient: e.user, issue: child, typ: "new_comment", actorType: "agent", actorID: e.agentA},
		"not about an issue":    {recipient: e.user, typ: "quick_create_done"},
	} {
		t.Run(name, func(t *testing.T) { e.writeInbox(w) })
	}

	got := e.holds()
	if len(got) != 1 {
		t.Fatalf("holds = %+v, want only the mention by another agent", got)
	}
	x := got[0]
	if x.InboxItemID != taken || x.SubscriptionID != sub.ID || x.AgentID != e.agentA || x.RecipientUserID != e.user ||
		x.IssueID != child || x.Mode != HoldModeShadow || x.Status != HoldStatusShadow || !x.InInbox ||
		x.ItemType != "mentioned" || x.DispatchTaskID != nil {
		t.Fatalf("hold = %+v", x)
	}
	var details string
	if err := pool.QueryRow(e.ctx, `SELECT item->'details'->>'k' FROM router_hold WHERE id = $1`, x.ID).Scan(&details); err != nil || details != "v" {
		t.Fatalf("stored item details = %q (%v)", details, err)
	}

	st := h.Stats()
	if st.Calls != 7 || st.WouldHold != 1 || st.Errors != 0 || st.Timeouts != 0 || st.Panics != 0 {
		t.Fatalf("stats = %+v", st)
	}
	var bucketed int64
	for _, n := range st.Latency {
		bucketed += n
	}
	if bucketed != st.Calls {
		t.Fatalf("latency buckets count %d calls, want %d", bucketed, st.Calls)
	}
}

func TestShadowHoldNeedsAnActiveChatBoundSubscription(t *testing.T) {
	e := newEnv(t)
	root := e.issue("router root")
	unbound := e.subscribe(e.agentA, ScopeIssueTree, root)
	_ = unbound
	paused := e.subscribe(e.agentB, ScopeIssueTree, root)
	e.bindChat(paused)
	if _, err := e.store.Pause(e.ctx, e.ws, paused.ID, testChange); err != nil {
		t.Fatal(err)
	}
	e.shadowHold()
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned"})
	if got := e.holds(); len(got) != 0 {
		t.Fatalf("holds = %+v, want none: one subscription has no chat, the other is paused", got)
	}

	// Resumed, but its chat archived: still nothing.
	if _, err := e.store.Resume(e.ctx, e.ws, paused.ID, testChange); err != nil {
		t.Fatal(err)
	}
	chat := *e.get(paused.ID).AnchorChatSessionID
	e.fx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, chat)
	subscriptionGeneration.Add(1)
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned"})
	if got := e.holds(); len(got) != 0 {
		t.Fatalf("holds = %+v, want none with the chat archived", got)
	}
}

func TestShadowHoldPicksTheNarrowestSubscription(t *testing.T) {
	e := newEnv(t)
	root := e.issue("router root")
	child := e.issue("router child", testutil.Cols{"parent_issue_id": root})
	leaf := e.issue("router leaf", testutil.Cols{"parent_issue_id": child})
	wide := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(wide)
	narrow := e.subscribe(e.agentB, ScopeIssueTree, child)
	e.bindChat(narrow)
	e.shadowHold()

	e.writeInbox(inboxWrite{recipient: e.user, issue: leaf, typ: "status_changed"})
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "status_changed"})
	got := e.holds()
	bySub := map[string]string{}
	for _, x := range got {
		bySub[x.IssueID] = x.SubscriptionID
	}
	if len(got) != 2 || bySub[leaf] != narrow.ID || bySub[root] != wide.ID {
		t.Fatalf("holds = %+v, want the leaf under %s and the root under %s", got, narrow.ID, wide.ID)
	}
}

// failingQuerier answers every query with err, or blocks until the context
// ends when block is set.
type failingQuerier struct {
	err   error
	block bool
}

func (f failingQuerier) Exec(ctx context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.wait(ctx)
}
func (f failingQuerier) Query(ctx context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, f.wait(ctx)
}
func (f failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (f failingQuerier) wait(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func TestShadowHoldFailsOpen(t *testing.T) {
	e := newEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)

	// Load the cache with the working database, then break it: the write
	// still lands and the failure is counted.
	h := NewHoldShadow(e.store)
	h.subscriptions(e.ctx)
	h.Store = &Store{DB: failingQuerier{err: errors.New("database down")}}
	inbox.SetInterceptor(h, 5*time.Second)
	t.Cleanup(func() {
		inbox.SetInterceptor(nil, 0)
		e.fx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1`, e.ws)
	})
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned"})
	if st := h.Stats(); st.Errors != 1 || st.WouldHold != 0 {
		t.Fatalf("stats after a database error = %+v", st)
	}

	// A database that hangs costs the write at most the budget.
	h.Store = &Store{DB: failingQuerier{block: true}}
	inbox.SetInterceptor(h, 50*time.Millisecond)
	start := time.Now()
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned"})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the write waited %s", took)
	}
	// The hook stops waiting at the budget; the call itself records its
	// timeout when it returns, on its own goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for h.Stats().Timeouts != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := h.Stats(); st.Timeouts != 1 {
		t.Fatalf("stats after a hang = %+v", st)
	}
	if got := e.holds(); len(got) != 0 {
		t.Fatalf("holds = %+v", got)
	}
}

func TestChatBatchListsShadowHolds(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat, d.HoldShadow = true, true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)
	e.shadowHold()

	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned", actorType: "agent", actorID: e.agentB})
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.calls))
	}
	note := runs.calls[0].note
	if !strings.Contains(note, "Hold preview (shadow mode, nothing to do): 1 inbox item(s)") ||
		!strings.Contains(note, "RT-") || !strings.Contains(note, "mentioned by agent router-b: item mentioned") {
		t.Fatalf("batch:\n%s", note)
	}
	got := e.holds()
	if len(got) != 1 || got[0].DispatchTaskID == nil || *got[0].DispatchTaskID != runs.calls[0].task {
		t.Fatalf("holds after the batch = %+v", got)
	}

	// A failed turn lists them again; a completed one does not.
	e.setTask(runs.calls[0].task, "failed")
	e.tick(d)
	if len(runs.calls) != 2 || !strings.Contains(runs.calls[1].note, "Hold preview") {
		t.Fatalf("after the failed turn: runs = %d", len(runs.calls))
	}
	e.setTask(runs.calls[1].task, "completed")
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 3 || strings.Contains(runs.calls[2].note, "Hold preview") {
		t.Fatalf("third batch lists holds again:\n%s", runs.calls[len(runs.calls)-1].note)
	}
}

func TestChatBatchOmitsShadowHoldsWithTheFeatureOff(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)
	e.shadowHold()
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned"})
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 || strings.Contains(runs.calls[0].note, "Hold preview") {
		t.Fatalf("runs = %+v", runs.calls)
	}
}

func TestPruneShadowHolds(t *testing.T) {
	e := newEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)
	e.shadowHold()
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "mentioned"})
	e.writeInbox(inboxWrite{recipient: e.user, issue: root, typ: "status_changed"})
	e.fx.Exec(t, `UPDATE router_hold SET created_at = now() - interval '31 days' WHERE workspace_id = $1 AND item_type = 'mentioned'`, e.ws)
	if _, err := pruneShadowHolds(e.ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := e.holds(); len(got) != 1 || got[0].ItemType != "status_changed" {
		t.Fatalf("holds after pruning = %+v", got)
	}
}
