// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).

package agentrouter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeRuns stands in for the task service: it queues a plain task row so
// settle sees a real run, and can be told to refuse or report busy.
type fakeRuns struct {
	e     *env
	err   error
	calls []fakeRun
}

type fakeRun struct{ issue, agent, note, task string }

func (f *fakeRuns) EnqueueRouterRun(_ context.Context, issueID, agentID, note string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	task := f.e.fx.Task(f.e.t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": f.e.runtimeOf(agentID)})
	f.calls = append(f.calls, fakeRun{issue: issueID, agent: agentID, note: note, task: task})
	return task, nil
}

func (e *env) runtimeOf(agentID string) string {
	e.t.Helper()
	var runtime string
	if err := pool.QueryRow(e.ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtime); err != nil {
		e.t.Fatal(err)
	}
	return runtime
}

var testChange = Change{ActorType: "system"}

func newDispatchEnv(t *testing.T) (*env, *Dispatcher, *fakeRuns) {
	e := newEnv(t)
	runs := &fakeRuns{e: e}
	d := NewDispatcher(e.store, runs, NewNotifier(e.store, InboxAdapter{Queries: db.New(pool)}))
	t.Cleanup(func() {
		e.fx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1`, e.ws)
		e.fx.Exec(t, `DELETE FROM agent_task_queue WHERE agent_id = ANY($1::uuid[])`, []string{e.agentA, e.agentB})
	})
	return e, d, runs
}

// tick runs dispatch for this test's workspace only: other packages share
// the database.
func (e *env) tick(d *Dispatcher) {
	e.t.Helper()
	if _, err := d.TickWorkspace(e.ctx, e.ws); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

// age makes a subscription's pending events old enough to be due.
func (e *env) age(subID string) {
	e.t.Helper()
	e.fx.Exec(e.t, `UPDATE router_event SET created_at = created_at - interval '2 hours' WHERE subscription_id = $1`, subID)
}

func (e *env) pending(subID string) []Event {
	e.t.Helper()
	evs, err := e.store.ListEvents(e.ctx, EventFilter{WorkspaceID: e.ws, SubscriptionID: subID, State: "pending", Limit: 500})
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *env) openDispatches(agent string) int {
	e.t.Helper()
	var n int
	if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM router_dispatch WHERE agent_id = $1 AND settled_at IS NULL`, agent).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) setTask(taskID, status string) {
	e.t.Helper()
	e.fx.Exec(e.t, `UPDATE agent_task_queue SET status = $2, completed_at = clock_timestamp() WHERE id = $1`, taskID, status)
}

func TestDispatchBatchesEventsIntoOneAnchorRun(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	child := e.issue("router child", testutil.Cols{"parent_issue_id": root})
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)

	e.publishComment(e.comment(child))
	e.setStatus(child, "in_progress")
	e.publishIssue(protocol.EventIssueUpdated, child, "member", e.user)
	// The agent's own comment is captured but never starts a run.
	e.publishComment(e.comment(child, testutil.Cols{"author_type": "agent", "author_id": e.agentA}))

	e.tick(d)
	if len(runs.calls) != 0 {
		t.Fatalf("dispatched before the debounce passed: %v", runs.calls)
	}

	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.calls))
	}
	run := runs.calls[0]
	if run.issue != root || run.agent != e.agentA {
		t.Fatalf("run on %s for %s, want the anchor %s for agent A", run.issue, run.agent, root)
	}
	for _, want := range []string{"Router batch for subscription " + sub.ID, "2 new event(s)", "comment_created", "status_changed todo -> in_progress", "Open issues in scope", "State (revision 0)"} {
		if !strings.Contains(run.note, want) {
			t.Fatalf("note lacks %q:\n%s", want, run.note)
		}
	}
	if strings.Count(run.note, "comment_created") != 1 {
		t.Fatalf("note lists the agent's own comment:\n%s", run.note)
	}
	if p := e.pending(sub.ID); len(p) != 0 {
		t.Fatalf("pending after dispatch = %v, want every event, own ones too, consumed", p)
	}
	for _, ev := range e.events(sub.ID) {
		if ev.ConsumedTaskID == nil || *ev.ConsumedTaskID != run.task {
			t.Fatalf("event %s consumed by %v, want %s", ev.Kind, ev.ConsumedTaskID, run.task)
		}
	}

	// A new event while the run is open waits.
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("dispatched while the agent's run was open")
	}
	// Once the run completes, the next batch goes.
	e.setTask(run.task, "completed")
	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d after completion, want 2", len(runs.calls))
	}
	var outcome string
	if err := pool.QueryRow(e.ctx, `SELECT outcome FROM router_dispatch WHERE task_id = $1`, run.task).Scan(&outcome); err != nil || outcome != "completed" {
		t.Fatalf("first dispatch outcome = %q (%v), want completed", outcome, err)
	}
}

func TestOwnEventsAloneNeverDispatch(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.publishComment(e.comment(root, testutil.Cols{"author_type": "agent", "author_id": e.agentA}))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 0 || len(e.pending(sub.ID)) != 1 {
		t.Fatalf("own event: runs = %d, pending = %d; want no run and the event still pending", len(runs.calls), len(e.pending(sub.ID)))
	}
}

// TestOwnEventsBacklogDoesNotHoldBackOthers: more of the agent's own events
// than one batch takes, ahead of one event someone else caused, still give a
// run, and every one of them is consumed with it.
func TestOwnEventsBacklogDoesNotHoldBackOthers(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	for range maxBatchEvents + 50 {
		e.publishComment(e.comment(root, testutil.Cols{"author_type": "agent", "author_id": e.agentA}))
	}
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want 1: own events held the batch back", len(runs.calls))
	}
	if p := e.pending(sub.ID); len(p) != 0 {
		t.Fatalf("pending after dispatch = %d, want 0", len(p))
	}
	if !strings.Contains(runs.calls[0].note, "1 new event(s)") {
		t.Fatalf("note:\n%s", runs.calls[0].note)
	}
}

// TestFullBatchLeavesLaterEventsForTheNext: a batch cut at the limit takes
// the oldest events others caused and the own events before the last of
// them; everything after waits for the next batch.
func TestFullBatchLeavesLaterEventsForTheNext(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.publishComment(e.comment(root, testutil.Cols{"author_type": "agent", "author_id": e.agentA}))
	for range maxBatchEvents + 1 {
		e.publishComment(e.comment(root))
	}
	e.publishComment(e.comment(root, testutil.Cols{"author_type": "agent", "author_id": e.agentA}))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.calls))
	}
	p := e.pending(sub.ID)
	self := 0
	for _, ev := range p {
		if ev.Self {
			self++
		}
	}
	if len(p) != 2 || self != 1 {
		t.Fatalf("pending = %d (%d own), want the last outside event and the own one after it", len(p), self)
	}
}

func TestFailedRunHandsTheBatchBack(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	first := runs.calls[0].task

	// A system retry keeps the dispatch open until the retry ends.
	e.setTask(first, "failed")
	retry := e.fx.Task(t, e.agentA, testutil.Cols{"issue_id": root, "parent_task_id": first, "retry_of_task_id": first, "runtime_id": e.runtimeOf(e.agentA)})
	e.tick(d)
	if e.openDispatches(e.agentA) != 1 || len(runs.calls) != 1 {
		t.Fatalf("settled or redispatched while a retry was pending")
	}

	// The retry fails too: the batch goes back and is dispatched again.
	e.setTask(retry, "cancelled")
	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d, want the batch dispatched again", len(runs.calls))
	}
	var outcome string
	if err := pool.QueryRow(e.ctx, `SELECT outcome FROM router_dispatch WHERE task_id = $1`, first).Scan(&outcome); err != nil || outcome != "failed" {
		t.Fatalf("outcome = %q (%v), want failed", outcome, err)
	}
	evs := e.events(sub.ID)
	if len(evs) != 1 || evs[0].ConsumedTaskID == nil || *evs[0].ConsumedTaskID != runs.calls[1].task {
		t.Fatalf("event after redispatch = %+v", evs)
	}
}

func TestOneRunAtATimePerAgent(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	rootA := e.issue("router root a")
	rootB := e.issue("router root b")
	subA := e.subscribe(e.agentA, ScopeIssueTree, rootA)
	subB := e.subscribe(e.agentA, ScopeIssueTree, rootB)
	e.publishComment(e.comment(rootA))
	e.publishComment(e.comment(rootB))
	e.age(subA.ID)
	e.age(subB.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want one: the agent's two subscriptions run one at a time", len(runs.calls))
	}
	e.setTask(runs.calls[0].task, "completed")
	e.tick(d)
	if len(runs.calls) != 2 || runs.calls[0].issue == runs.calls[1].issue {
		t.Fatalf("second run = %+v, want the other anchor", runs.calls)
	}
}

func TestRateLimitPausesDispatchAndNotifiesTheOwner(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{Batching: Batching{MaxRunsPerHour: 1}, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	e.setTask(runs.calls[0].task, "completed")

	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("dispatched past the hourly limit")
	}
	got := e.get(sub.ID)
	if !got.Enabled || got.PausedReason == nil || *got.PausedReason != PausedRate {
		t.Fatalf("subscription = enabled %v reason %v, want capture on and dispatch paused for rate", got.Enabled, got.PausedReason)
	}
	var title, typ string
	if err := pool.QueryRow(e.ctx, `
		SELECT title, type FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2`, e.ws, e.user).Scan(&title, &typ); err != nil {
		t.Fatalf("owner inbox: %v", err)
	}
	if typ != InboxNoticeType || !strings.Contains(title, "Router paused") {
		t.Fatalf("inbox item = %q %q", typ, title)
	}
	notices, err := d.Notices.ListNotices(e.ctx, NoticeFilter{WorkspaceID: e.ws, SubscriptionID: sub.ID})
	if err != nil || len(notices) != 1 || notices[0].ChannelStates["inbox"].State != DeliverySent {
		t.Fatalf("notices = %+v (%v)", notices, err)
	}
	changes, _ := e.store.ListChanges(e.ctx, e.ws, sub.ID)
	if last := changes[len(changes)-1]; last.Action != ChangeAutoPause || last.ActorType != "system" {
		t.Fatalf("last change = %+v", last)
	}

	// Capture went on; a resume dispatches what arrived meanwhile once the
	// hour allows it.
	e.publishComment(e.comment(root))
	if len(e.pending(sub.ID)) != 2 {
		t.Fatalf("capture stopped while dispatch was paused")
	}
	if _, err := e.store.Resume(e.ctx, e.ws, sub.ID, Change{ActorType: "member", ActorID: e.user}); err != nil {
		t.Fatal(err)
	}
	e.fx.Exec(t, `UPDATE router_dispatch SET created_at = created_at - interval '2 hours' WHERE subscription_id = $1`, sub.ID)
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d after resume, want 2", len(runs.calls))
	}
}

func TestUnavailableOrRefusedRunsPauseDispatch(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	rootA := e.issue("router root a")
	rootB := e.issue("router root b")
	subA := e.subscribe(e.agentA, ScopeIssueTree, rootA)
	subB := e.subscribe(e.agentB, ScopeIssueTree, rootB)

	e.fx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, e.agentA)
	e.publishComment(e.comment(rootA))
	e.age(subA.ID)
	e.tick(d)
	if r := e.get(subA.ID).PausedReason; r == nil || *r != PausedAgentUnavailable {
		t.Fatalf("archived agent: paused_reason = %v", r)
	}

	runs.err = fmt.Errorf("%w: private project", ErrRunRefused)
	e.publishComment(e.comment(rootB))
	e.age(subB.ID)
	e.tick(d)
	if r := e.get(subB.ID).PausedReason; r == nil || *r != PausedAgentUnavailable {
		t.Fatalf("refused run: paused_reason = %v", r)
	}

	// Busy means wait, not pause.
	runs.err = ErrRunBusy
	if _, err := e.store.Resume(e.ctx, e.ws, subB.ID, Change{ActorType: "member", ActorID: e.user}); err != nil {
		t.Fatal(err)
	}
	e.tick(d)
	if r := e.get(subB.ID).PausedReason; r != nil {
		t.Fatalf("busy run paused the subscription: %v", *r)
	}
	if len(e.pending(subB.ID)) != 1 {
		t.Fatalf("busy run lost the batch")
	}
}

func TestProjectScopeWithoutAnchorNeverDispatches(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	project := e.fx.Project(t, "router project")
	issue := e.issue("router project issue", testutil.Cols{"project_id": project})
	sub := e.subscribe(e.agentA, ScopeProject, project)
	e.publishComment(e.comment(issue))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 0 {
		t.Fatalf("dispatched a project subscription without an anchor")
	}
	anchor := issue
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{AnchorIssueID: &anchor, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	e.tick(d)
	if len(runs.calls) != 1 || runs.calls[0].issue != issue {
		t.Fatalf("runs = %+v, want one on the new anchor", runs.calls)
	}
}

func TestStateRevisionAndLimit(t *testing.T) {
	e := newEnv(t)
	sub := e.subscribe(e.agentA, ScopeIssueTree, e.issue("router root"))
	st, err := e.store.SetState(e.ctx, e.ws, sub.ID, "first", 0)
	if err != nil || st.Revision != 1 {
		t.Fatalf("set = %+v (%v)", st, err)
	}
	if _, err := e.store.SetState(e.ctx, e.ws, sub.ID, "stale", 0); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale revision: %v, want ErrStateConflict", err)
	}
	if _, err := e.store.SetState(e.ctx, e.ws, sub.ID, strings.Repeat("x", MaxStateBytes+1), 1); !errors.Is(err, ErrStateTooLarge) {
		t.Fatalf("oversized state: %v, want ErrStateTooLarge", err)
	}
	got, err := e.store.GetState(e.ctx, e.ws, sub.ID)
	if err != nil || got.State != "first" || got.Revision != 1 {
		t.Fatalf("get = %+v (%v)", got, err)
	}
	if s := e.get(sub.ID); s.StateRevision != 1 || s.StateBytes != 5 {
		t.Fatalf("subscription state fields = %d/%d", s.StateRevision, s.StateBytes)
	}
}

func TestNoticeDedupeAndDelivery(t *testing.T) {
	e, d, _ := newDispatchEnv(t)
	root := e.issue("router root")
	in := NoticeInput{
		WorkspaceID: e.ws, RecipientUserID: e.user, Title: "Decide on RT", Body: "Pick A or B",
		Refs: []string{root}, Urgency: UrgencyHigh, DedupeKey: "decide-1", SenderType: "agent", SenderID: e.agentA,
	}
	n, created, err := d.Notices.Send(e.ctx, in)
	if err != nil || !created || n.ChannelStates["inbox"].State != DeliverySent {
		t.Fatalf("send = %+v created %v (%v)", n, created, err)
	}
	again, created, err := d.Notices.Send(e.ctx, in)
	if err != nil || created || again.ID != n.ID {
		t.Fatalf("resend = %s created %v (%v), want the first notice", again.ID, created, err)
	}
	// The same key from another sender, or to another recipient, is a new
	// notice and reveals nothing of the first.
	other := e.fx.User(t, "router other", "router-other-"+e.ws[:8]+"@example.com")
	e.fx.Member(t, e.ws, other, "member")
	for name, in2 := range map[string]NoticeInput{
		"another agent":               {SenderType: "agent", SenderID: e.agentB, RecipientUserID: e.user},
		"a member":                    {SenderType: "member", SenderID: other, RecipientUserID: other},
		"the system":                  {SenderType: "system", RecipientUserID: e.user},
		"same agent, other recipient": {SenderType: "agent", SenderID: e.agentA, RecipientUserID: other},
	} {
		in2.WorkspaceID, in2.Title, in2.DedupeKey = e.ws, "Mine", in.DedupeKey
		got, created, err := d.Notices.Send(e.ctx, in2)
		if err != nil || !created || got.ID == n.ID || got.Body != "" {
			t.Fatalf("%s reusing the key = %+v created %v (%v), want a new notice of its own", name, got, created, err)
		}
	}
	// A system notice's key is taken only by the system.
	sys := NoticeInput{WorkspaceID: e.ws, RecipientUserID: e.user, Title: "paused", DedupeKey: "router_paused:x", SenderType: "system"}
	squat := sys
	squat.SenderType, squat.SenderID = "agent", e.agentB
	if _, created, err := d.Notices.Send(e.ctx, squat); err != nil || !created {
		t.Fatal(err)
	}
	if _, created, err := d.Notices.Send(e.ctx, sys); err != nil || !created {
		t.Fatalf("system notice suppressed by another sender's key: created %v (%v)", created, err)
	}

	var items int
	var severity, issueID string
	if err := pool.QueryRow(e.ctx, `
		SELECT count(*), min(severity), min(issue_id::text) FROM inbox_item
		WHERE workspace_id = $1 AND type = $2 AND title = $3`, e.ws, InboxNoticeType, in.Title).Scan(&items, &severity, &issueID); err != nil {
		t.Fatal(err)
	}
	if items != 1 || severity != "action_required" || issueID != root {
		t.Fatalf("inbox = %d items, severity %s, issue %s", items, severity, issueID)
	}

	// A notice written but never claimed (the process stopped after the
	// commit) is delivered by the next pass; one claimed but unfinished
	// (unknown) is not sent again.
	pendingIn := in
	pendingIn.DedupeKey, pendingIn.Title = "decide-2", "Pending"
	p, _, err := insertNotice(e.ctx, pool, pendingIn)
	if err != nil {
		t.Fatal(err)
	}
	unknownIn := in
	unknownIn.DedupeKey, unknownIn.Title = "decide-3", "Unknown"
	u, _, err := insertNotice(e.ctx, pool, unknownIn)
	if err != nil {
		t.Fatal(err)
	}
	e.fx.Exec(t, `UPDATE owner_notice SET channel_states = '{"inbox":{"state":"unknown"}}' WHERE id = $1`, u.ID)
	if _, err := d.Notices.DeliverPending(e.ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Notices.Get(e.ctx, e.ws, p.ID)
	if got.ChannelStates["inbox"].State != DeliverySent {
		t.Fatalf("pending notice = %+v", got.ChannelStates)
	}
	if got, _ := d.Notices.Get(e.ctx, e.ws, u.ID); got.ChannelStates["inbox"].State != DeliveryUnknown {
		t.Fatalf("unknown notice was retried: %+v", got.ChannelStates)
	}
}
