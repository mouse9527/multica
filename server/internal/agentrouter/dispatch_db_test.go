// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), Router v6 slice 1 (M-96), Router parallelism (M-104), and no rate pause (M-111).

package agentrouter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeRuns stands in for the task service: it queues a plain task row so
// settle sees a real run, and can be told to refuse or report busy. It also
// records the notices posted into Router chats.
type fakeRuns struct {
	e       *env
	err     error
	mu      sync.Mutex
	calls   []fakeRun
	notices []fakeRun
}

type fakeRun struct{ issue, chat, agent, note, task string }

func (f *fakeRuns) PostRouterChatNotice(_ context.Context, chatSessionID, agentID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, fakeRun{chat: chatSessionID, agent: agentID, note: content})
	return fmt.Sprintf("message-%d", len(f.notices)), nil
}

func (f *fakeRuns) EnqueueRouterChatRun(_ context.Context, chatSessionID, agentID, _, batch string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	task := f.e.fx.Task(f.e.t, agentID, testutil.Cols{"chat_session_id": chatSessionID, "runtime_id": f.e.runtimeOf(agentID)})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeRun{chat: chatSessionID, agent: agentID, note: batch, task: task})
	return task, nil
}

func (f *fakeRuns) EnqueueRouterRun(_ context.Context, issueID, agentID, note string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	task := f.e.fx.Task(f.e.t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": f.e.runtimeOf(agentID)})
	f.mu.Lock()
	defer f.mu.Unlock()
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
	d := NewDispatcher(e.store, runs, NewNotifier(e.store, ChatAdapter{Store: e.store, Poster: runs}, InboxAdapter{Queries: db.New(pool)}))
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
	for _, want := range []string{"Super Agent batch for subscription " + sub.ID, "2 new event(s)", "comment_created", "status_changed todo -> in_progress", "Open issues in scope", "State (revision 0)"} {
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
		t.Fatalf("dispatched while the subscription's run was open")
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

func TestBusyRouterKeepsDispatchingAndTellsTheOwnerOnce(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{Batching: Batching{MaxRunsPerHour: 1}, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		e.publishComment(e.comment(root))
		e.age(sub.ID)
		e.tick(d)
		if len(runs.calls) != i+1 {
			t.Fatalf("batch %d: runs = %d, want every batch dispatched past the threshold", i+1, len(runs.calls))
		}
		e.setTask(runs.calls[i].task, "completed")
	}
	if got := e.get(sub.ID); !got.Enabled || got.PausedReason != nil {
		t.Fatalf("subscription = enabled %v reason %v, want it still dispatching", got.Enabled, got.PausedReason)
	}

	// The second run went past the threshold; the third did not alert again.
	notices, err := d.Notices.ListNotices(e.ctx, NoticeFilter{WorkspaceID: e.ws, SubscriptionID: sub.ID})
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices = %+v (%v), want one", notices, err)
	}
	n := notices[0]
	if !strings.Contains(n.Title, "Super Agent busy") || !strings.Contains(n.Body, "started 2 runs in the last hour") ||
		!strings.Contains(n.Body, "nothing is paused") || n.Urgency != UrgencyNormal || n.RecipientUserID != e.user {
		t.Fatalf("notice = %+v", n)
	}
	// Not bound to a chat: the chat skips it and the inbox delivers it.
	if n.ChannelStates["chat"].State != DeliverySkipped || n.ChannelStates["inbox"].State != DeliverySent {
		t.Fatalf("channel states = %+v", n.ChannelStates)
	}
	var typ, severity string
	if err := pool.QueryRow(e.ctx, `
		SELECT type, severity FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2`, e.ws, e.user).Scan(&typ, &severity); err != nil {
		t.Fatalf("owner inbox: %v", err)
	}
	if typ != InboxNoticeType || severity != "info" {
		t.Fatalf("inbox item = %s %s", typ, severity)
	}
	changes, _ := e.store.ListChanges(e.ctx, e.ws, sub.ID)
	for _, c := range changes {
		if c.Action == ChangeAutoPause {
			t.Fatalf("auto pause recorded: %+v", c)
		}
	}
}

func TestBusyRouterAlertGoesToItsChat(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	chat := e.bindChat(sub)
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{Batching: Batching{MaxRunsPerHour: 1}, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		e.publishComment(e.comment(root))
		e.age(sub.ID)
		e.tick(d)
		e.setTask(runs.calls[i].task, "completed")
	}
	if len(runs.calls) != 2 || len(runs.notices) != 1 {
		t.Fatalf("runs = %d, chat notices = %d; want 2 and 1", len(runs.calls), len(runs.notices))
	}
	if got := runs.notices[0]; got.chat != chat || got.agent != e.agentA ||
		!strings.HasPrefix(got.note, RouterNoticeLabel+"\n") || !strings.Contains(got.note, "Super Agent busy") {
		t.Fatalf("chat notice = %+v", got)
	}
	notices, _ := d.Notices.ListNotices(e.ctx, NoticeFilter{WorkspaceID: e.ws, SubscriptionID: sub.ID})
	if len(notices) != 1 || notices[0].ChannelStates["chat"].State != DeliverySent ||
		notices[0].ChannelStates["inbox"].State != DeliverySkipped {
		t.Fatalf("notices = %+v, want sent to the chat and not to the inbox", notices)
	}
	var inbox int
	if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1`, e.ws).Scan(&inbox); err != nil || inbox != 0 {
		t.Fatalf("inbox items = %d (%v), want none", inbox, err)
	}
}

// TestRatePauseFromAnOlderReleaseIsLifted: a subscription an older release
// paused for "rate" dispatches again, with what it captured meanwhile.
func TestRatePauseFromAnOlderReleaseIsLifted(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	manual := e.subscribe(e.agentB, ScopeIssueTree, root)
	e.fx.Exec(t, `UPDATE router_subscription SET paused_reason = 'rate' WHERE id = $1`, sub.ID)
	if _, err := e.store.Pause(e.ctx, e.ws, manual.ID, testChange); err != nil {
		t.Fatal(err)
	}
	e.publishComment(e.comment(root))
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if got := e.get(sub.ID); got.PausedReason != nil || !got.Enabled {
		t.Fatalf("subscription = enabled %v reason %v, want dispatching again", got.Enabled, got.PausedReason)
	}
	if len(runs.calls) != 1 || runs.calls[0].agent != e.agentA || !strings.Contains(runs.calls[0].note, "2 new event(s)") {
		t.Fatalf("runs = %+v, want the backlog dispatched as one batch", runs.calls)
	}
	changes, _ := e.store.ListChanges(e.ctx, e.ws, sub.ID)
	if last := changes[len(changes)-1]; last.Action != ChangeResume || last.ActorType != "system" || last.Detail["reason"] != PausedRate {
		t.Fatalf("last change = %+v", last)
	}
	if got := e.get(manual.ID); got.Enabled || got.PausedReason == nil || *got.PausedReason != PausedManual {
		t.Fatalf("manual pause = enabled %v reason %v, want it kept", got.Enabled, got.PausedReason)
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
