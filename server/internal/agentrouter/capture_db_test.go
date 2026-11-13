// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// env is one isolated workspace with two agents. Every test builds its own,
// so tests never read each other's rows.
type env struct {
	t       *testing.T
	ctx     context.Context
	fx      *testutil.Fixture
	store   *Store
	capt    *Capturer
	recon   *Reconciler
	ws      string
	user    string
	agentA  string
	agentB  string
	counter int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), url) })
	if poolErr != nil {
		t.Fatalf("connect: %v", poolErr)
	}
	ctx := context.Background()
	fx := testutil.New(pool, "", "")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	user := fx.User(t, "router test", "router-"+suffix+"@example.test")
	ws := fx.Workspace(t, "router test", "router-"+suffix, testutil.Cols{"issue_prefix": "RT"})
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	runtime := fx.Runtime(t, "router-runtime")
	e := &env{
		t: t, ctx: ctx, fx: fx, ws: ws, user: user,
		agentA: fx.Agent(t, "router-a", runtime),
		agentB: fx.Agent(t, "router-b", runtime),
	}
	e.store = NewStore(pool, pool)
	e.capt = NewCapturer(e.store)
	e.capt.Timeout = 5 * time.Second
	e.recon = NewReconciler(e.store)
	e.recon.Grace = 0
	t.Cleanup(func() { _ = DeleteWorkspaceData(context.Background(), pool, ws) })
	return e
}

func (e *env) issue(title string, over ...testutil.Cols) string {
	e.t.Helper()
	return e.fx.Issue(e.t, title, over...)
}

func (e *env) subscribe(agent, scopeType, scopeID string, kinds ...string) Subscription {
	e.t.Helper()
	types, err := NormalizeEventTypes(kinds)
	if err != nil {
		e.t.Fatal(err)
	}
	sub, err := e.store.Create(e.ctx, CreateParams{
		WorkspaceID: e.ws, AgentID: agent, OwnerUserID: e.user,
		ScopeType: scopeType, ScopeID: scopeID, EventTypes: types,
		CreatedByType: "member", CreatedByID: e.user,
	})
	if err != nil {
		e.t.Fatalf("subscribe: %v", err)
	}
	return sub
}

func (e *env) comment(issueID string, over ...testutil.Cols) string {
	e.t.Helper()
	e.counter++
	return e.fx.Comment(e.t, issueID, fmt.Sprintf("comment %d", e.counter), over...)
}

func (e *env) setStatus(issueID, status string) {
	e.t.Helper()
	e.fx.Exec(e.t, `UPDATE issue SET status = $2, updated_at = clock_timestamp() WHERE id = $1`, issueID, status)
}

func (e *env) setAssignee(issueID, typ, id string) {
	e.t.Helper()
	e.fx.Exec(e.t, `UPDATE issue SET assignee_type = $2, assignee_id = $3, updated_at = clock_timestamp() WHERE id = $1`, issueID, typ, id)
}

func (e *env) failedTask(agent, issueID string, over ...testutil.Cols) string {
	e.t.Helper()
	cols := testutil.Cols{"issue_id": issueID, "status": "failed", "completed_at": testutil.Raw("clock_timestamp()"), "failure_reason": "agent_error"}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	return e.fx.Task(e.t, agent, cols)
}

// Publish shapes, as the server's publishers send them.

func (e *env) publishIssue(eventType, issueID, actorType, actorID string) {
	e.capt.Handle(events.Event{Type: eventType, WorkspaceID: e.ws, ActorType: actorType, ActorID: actorID,
		Payload: map[string]any{"issue": map[string]any{"id": issueID}}})
}

func (e *env) publishComment(commentID string) {
	e.capt.Handle(events.Event{Type: protocol.EventCommentCreated, WorkspaceID: e.ws, ActorType: "member", ActorID: e.user,
		Payload: map[string]any{"comment": map[string]any{"id": commentID}}})
}

func (e *env) publishTaskFailed(taskID, issueID, agent string, retryPending bool) {
	e.capt.Handle(events.Event{Type: protocol.EventTaskFailed, WorkspaceID: e.ws, ActorType: "system",
		Payload: map[string]any{"task_id": taskID, "issue_id": issueID, "agent_id": agent, "retry_pending": retryPending}})
}

func (e *env) events(subID string) []Event {
	e.t.Helper()
	evs, err := e.store.ListEvents(e.ctx, EventFilter{WorkspaceID: e.ws, SubscriptionID: subID, Limit: 500})
	if err != nil {
		e.t.Fatalf("list events: %v", err)
	}
	return evs
}

// kinds summarizes events as "kind@issue/source", sorted.
func kinds(evs []Event, label map[string]string) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind+"@"+label[ev.IssueID]+"/"+ev.Source)
	}
	sort.Strings(out)
	return out
}

func assertKinds(t *testing.T, what string, got []Event, label map[string]string, want ...string) {
	t.Helper()
	sort.Strings(want)
	g := kinds(got, label)
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Fatalf("%s events = %v, want %v", what, g, want)
	}
}

func (e *env) reconcile() ReconcileResult {
	e.t.Helper()
	res, err := e.recon.RunWorkspace(e.ctx, e.ws)
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (e *env) get(id string) Subscription {
	e.t.Helper()
	sub, err := e.store.Get(e.ctx, e.ws, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return sub
}

// TestListenerCapturesInScopeEvents: every captured kind lands once for an
// issue in the subscribed tree, nothing lands for an issue outside it, and a
// following reconcile finds nothing missed.
func TestListenerCapturesInScopeEvents(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	child := e.issue("child", testutil.Cols{"parent_issue_id": root})
	outside := e.issue("outside")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	label := map[string]string{root: "root", child: "child", outside: "outside"}

	e.publishComment(e.comment(child))
	e.publishComment(e.comment(outside))

	e.setStatus(child, "in_progress")
	e.publishIssue(protocol.EventIssueUpdated, child, "member", e.user)
	e.setStatus(outside, "in_progress")
	e.publishIssue(protocol.EventIssueUpdated, outside, "member", e.user)

	grandchild := e.issue("grandchild", testutil.Cols{"parent_issue_id": child})
	label[grandchild] = "grandchild"
	e.publishIssue(protocol.EventIssueCreated, grandchild, "member", e.user)

	task := e.failedTask(e.agentB, child)
	e.publishTaskFailed(task, child, e.agentB, false)

	e.setAssignee(child, "agent", e.agentA)
	e.publishIssue(protocol.EventIssueUpdated, child, "agent", e.agentA)

	evs := e.events(sub.ID)
	assertKinds(t, "listener", evs, label,
		"comment_created@child/bus", "status_changed@child/bus", "issue_created@grandchild/bus",
		"task_failed@child/bus", "assignee_changed@child/bus")
	for _, ev := range evs {
		switch ev.Kind {
		case KindStatusChanged:
			if ev.Payload["from"] != "todo" || ev.Payload["to"] != "in_progress" || ev.Self {
				t.Errorf("status event = %+v", ev)
			}
		case KindAssigneeChanged:
			// The subscribed agent made this change itself.
			if !ev.Self {
				t.Errorf("assignee change by the subscribed agent not marked self: %+v", ev)
			}
		case KindTaskFailed:
			if ev.Self || ev.ActorID == nil || *ev.ActorID != e.agentB {
				t.Errorf("task event = %+v", ev)
			}
		}
	}

	if res := e.reconcile(); res.Total() != 0 {
		t.Fatalf("reconcile after full capture added %v", res.Missed)
	}
	if got := e.get(sub.ID); got.CaptureMiss != 0 || got.EventCount != 5 {
		t.Fatalf("event_count=%d capture_miss=%d, want 5 and 0", got.EventCount, got.CaptureMiss)
	}
}

// TestReconcileAddsWhatTheListenerMissed is the acceptance case: changes that
// never reach the listener are added by one reconcile pass and counted as
// capture_miss, and a second pass adds nothing.
func TestReconcileAddsWhatTheListenerMissed(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)

	e.comment(root)
	e.setStatus(root, "in_progress")
	e.setAssignee(root, "member", e.user)
	newChild := e.issue("new child", testutil.Cols{"parent_issue_id": root})
	e.failedTask(e.agentB, root)
	// A failure that was retried is not terminal and is never an event.
	retried := e.failedTask(e.agentB, newChild)
	e.fx.Task(t, e.agentB, testutil.Cols{"issue_id": newChild, "retry_of_task_id": retried, "status": "cancelled", "completed_at": testutil.Raw("now()")})

	res := e.reconcile()
	if res.Total() != 5 {
		t.Fatalf("reconcile capture_miss = %d (%v), want 5", res.Total(), res.Missed)
	}
	label := map[string]string{root: "root", newChild: "new"}
	assertKinds(t, "reconcile", e.events(sub.ID), label,
		"comment_created@root/reconcile", "status_changed@root/reconcile", "assignee_changed@root/reconcile",
		"issue_created@new/reconcile", "task_failed@root/reconcile")
	if got := e.get(sub.ID).CaptureMiss; got != 5 {
		t.Fatalf("capture_miss = %d, want 5", got)
	}

	if res := e.reconcile(); res.Total() != 0 {
		t.Fatalf("second reconcile added %v", res.Missed)
	}
}

// TestNarrowestScopeAcrossSubscriptions: an agent with overlapping
// subscriptions gets each event once, on its narrowest one; another agent
// gets its own copy. The listener (Go) and reconcile (SQL) agree.
func TestNarrowestScopeAcrossSubscriptions(t *testing.T) {
	e := newEnv(t)
	project := e.fx.Project(t, "router project")
	root := e.issue("root", testutil.Cols{"project_id": project})
	child := e.issue("child", testutil.Cols{"project_id": project, "parent_issue_id": root})
	other := e.issue("other", testutil.Cols{"project_id": project})
	aProject := e.subscribe(e.agentA, ScopeProject, project)
	aTree := e.subscribe(e.agentA, ScopeIssueTree, root)
	bProject := e.subscribe(e.agentB, ScopeProject, project)
	label := map[string]string{root: "root", child: "child", other: "other"}

	e.publishComment(e.comment(child))
	e.publishComment(e.comment(other))
	// Missed by the listener; reconcile must route it the same way.
	e.comment(child)
	if res := e.reconcile(); res.Total() != 2 {
		t.Fatalf("reconcile added %v, want one comment for each agent", res.Missed)
	}

	assertKinds(t, "agent A tree", e.events(aTree.ID), label,
		"comment_created@child/bus", "comment_created@child/reconcile")
	assertKinds(t, "agent A project", e.events(aProject.ID), label,
		"comment_created@other/bus")
	assertKinds(t, "agent B project", e.events(bProject.ID), label,
		"comment_created@child/bus", "comment_created@other/bus", "comment_created@child/reconcile")

	scope, err := e.store.Scope(e.ctx, e.ws, aProject.ID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) != 1 || scope[0].ID != other {
		t.Fatalf("agent A project scope = %+v, want only the issue outside the tree", scope)
	}
}

// TestEventKeyIsIdempotent: the same change offered twice, by the listener
// or by reconcile, is stored once.
func TestEventKeyIsIdempotent(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	c := e.comment(root)
	e.publishComment(c)
	e.publishComment(c)
	task := e.failedTask(e.agentB, root)
	e.publishTaskFailed(task, root, e.agentB, false)
	e.publishTaskFailed(task, root, e.agentB, false)
	if res := e.reconcile(); res.Total() != 0 {
		t.Fatalf("reconcile re-added captured events: %v", res.Missed)
	}
	if n := len(e.events(sub.ID)); n != 2 {
		t.Fatalf("%d events, want 2", n)
	}
}

// TestEventTypesFilter: kinds a subscription did not ask for are dropped by
// both paths.
func TestEventTypesFilter(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root, KindStatusChanged)
	e.publishComment(e.comment(root))
	e.comment(root)
	e.setStatus(root, "in_review")
	e.publishIssue(protocol.EventIssueUpdated, root, "member", e.user)
	e.reconcile()
	assertKinds(t, "filtered", e.events(sub.ID), map[string]string{root: "root"}, "status_changed@root/bus")
}

// TestPauseAndResume: nothing is captured while paused, and resuming starts
// from a fresh baseline, so changes made while paused are not misses.
func TestPauseAndResume(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	if _, err := e.store.Pause(e.ctx, e.ws, sub.ID); err != nil {
		t.Fatal(err)
	}
	e.publishComment(e.comment(root))
	e.setStatus(root, "blocked")
	e.publishIssue(protocol.EventIssueUpdated, root, "member", e.user)
	if n := len(e.events(sub.ID)); n != 0 {
		t.Fatalf("paused subscription captured %d events", n)
	}
	if _, err := e.store.Resume(e.ctx, e.ws, sub.ID); err != nil {
		t.Fatal(err)
	}
	if res := e.reconcile(); res.Total() != 0 {
		t.Fatalf("reconcile after resume counted paused-time changes as misses: %v", res.Missed)
	}
	e.setStatus(root, "in_progress")
	e.publishIssue(protocol.EventIssueUpdated, root, "member", e.user)
	evs := e.events(sub.ID)
	assertKinds(t, "after resume", evs, map[string]string{root: "root"}, "status_changed@root/bus")
	if evs[0].Payload["from"] != "blocked" {
		t.Fatalf("status change measured against %v, want the state at resume (blocked)", evs[0].Payload["from"])
	}
}

// TestDuplicateSubscription: one subscription per agent and scope.
func TestDuplicateSubscription(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	e.subscribe(e.agentA, ScopeIssueTree, root)
	_, err := e.store.Create(e.ctx, CreateParams{
		WorkspaceID: e.ws, AgentID: e.agentA, ScopeType: ScopeIssueTree, ScopeID: root,
		EventTypes: AllKinds, CreatedByType: "member", CreatedByID: e.user,
	})
	if err != ErrDuplicate {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

type recordingRegistrar struct{ names []string }

func (r *recordingRegistrar) Register(spec scheduler.JobSpec) error {
	r.names = append(r.names, spec.Name)
	return nil
}

// TestRegisterFollowsTheFlag: with router_v1 off nothing is registered, which
// is what keeps the server identical to upstream.
func TestRegisterFollowsTheFlag(t *testing.T) {
	watched := []string{protocol.EventIssueCreated, protocol.EventIssueUpdated, protocol.EventCommentCreated, protocol.EventTaskFailed}

	bus := events.New()
	jobs := &recordingRegistrar{}
	if Register(context.Background(), nil, bus, nil, jobs) {
		t.Fatal("registered with the flag unset")
	}
	off := featureflag.NewStaticProvider()
	off.Set(featureflags.RouterV1, featureflag.Rule{Default: false})
	if Register(context.Background(), featureflag.NewService(off), bus, nil, jobs) {
		t.Fatal("registered with the flag off")
	}
	for _, et := range watched {
		if n := bus.SubscriberCount(et); n != 0 {
			t.Fatalf("%s has %d listeners with the flag off", et, n)
		}
	}
	if len(jobs.names) != 0 {
		t.Fatalf("jobs registered with the flag off: %v", jobs.names)
	}

	on := featureflag.NewStaticProvider()
	on.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	if !Register(context.Background(), featureflag.NewService(on), bus, nil, jobs) {
		t.Fatal("not registered with the flag on")
	}
	for _, et := range watched {
		if n := bus.SubscriberCount(et); n != 1 {
			t.Fatalf("%s has %d listeners with the flag on, want 1", et, n)
		}
	}
	if fmt.Sprint(jobs.names) != "["+JobNameReconcile+"]" {
		t.Fatalf("jobs = %v", jobs.names)
	}
}
