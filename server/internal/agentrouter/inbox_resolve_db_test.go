// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package agentrouter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const waitingStatus = "started_2"

// inboxEnv is an env whose workspace has inbox resolve set up for its user.
type inboxEnv struct {
	*env
	resolver *InboxResolver
	archived []map[string]any
}

func newInboxEnv(t *testing.T) *inboxEnv {
	t.Helper()
	e := newEnv(t)
	cfg := InboxResolveConfig{OwnerUserID: e.user, WaitingStatuses: []string{waitingStatus}}
	if err := SetInboxResolveConfig(e.ctx, pool, e.ws, &cfg); err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	ie := &inboxEnv{env: e}
	bus.Subscribe(protocol.EventInboxBatchArchived, func(ev events.Event) {
		if ev.WorkspaceID == e.ws {
			ie.archived = append(ie.archived, ev.Payload.(map[string]any))
		}
	})
	ie.resolver = NewInboxResolver(pool, bus)
	ie.resolver.Timeout = 5 * time.Second
	return ie
}

// item writes an inbox item for recipient on issue, written ago before now.
func (e *inboxEnv) item(recipient, issueID string, ago time.Duration) string {
	e.t.Helper()
	return e.fx.Insert(e.t, "inbox_item", testutil.Cols{
		"workspace_id": e.ws, "recipient_type": "member", "recipient_id": recipient,
		"type": "status_changed", "title": "item", "issue_id": issueID,
		"created_at": testutil.Raw(fmt.Sprintf("now() - interval '%d seconds'", int(ago.Seconds()))),
	})
}

// activity records a status or assignee change on issue, ago before now, as
// the upstream activity listener writes it.
func (e *inboxEnv) activity(issueID, action, details string, ago time.Duration) {
	e.t.Helper()
	e.fx.Exec(e.t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details, created_at)
		VALUES ($1, $2, 'member', $3, $4::jsonb, now() - make_interval(secs => $5))`,
		e.ws, issueID, action, details, ago.Seconds())
}

func (e *inboxEnv) archivedItem(id string) bool {
	e.t.Helper()
	var archived bool
	if err := pool.QueryRow(e.ctx, `SELECT archived FROM inbox_item WHERE id = $1`, id).Scan(&archived); err != nil {
		e.t.Fatal(err)
	}
	return archived
}

func (e *inboxEnv) assertArchived(what string, want bool, ids ...string) {
	e.t.Helper()
	for _, id := range ids {
		if got := e.archivedItem(id); got != want {
			e.t.Fatalf("%s: item %s archived = %v, want %v", what, id, got, want)
		}
	}
}

// update publishes issue:updated the way the issue handlers shape it.
func (e *inboxEnv) update(issueID string, extra map[string]any) {
	e.t.Helper()
	cur, err := loadIssue(e.ctx, pool, e.ws, issueID)
	if err != nil {
		e.t.Fatal(err)
	}
	payload := map[string]any{"issue": map[string]any{
		"id": issueID, "status": cur.Status, "assignee_type": cur.AssigneeType, "assignee_id": cur.AssigneeID,
	}}
	for k, v := range extra {
		payload[k] = v
	}
	e.resolver.Handle(events.Event{Type: protocol.EventIssueUpdated, WorkspaceID: e.ws,
		ActorType: "member", ActorID: e.user, Payload: payload})
}

func strPtr(s string) *string { return &s }

func TestInboxResolveListenerArchivesWhenTheIssueLeavesTheWaitingStatus(t *testing.T) {
	e := newInboxEnv(t)
	left := e.issue("left", testutil.Cols{"status": waitingStatus})
	waiting := e.issue("still waiting", testutil.Cols{"status": waitingStatus})
	other := e.fx.User(t, "other", fmt.Sprintf("other-%d@example.test", time.Now().UnixNano()))
	e.fx.Member(t, e.ws, other, "member")

	mine := []string{e.item(e.user, left, time.Minute), e.item(e.user, left, 0)}
	theirs := e.item(other, left, 0)
	stays := e.item(e.user, waiting, 0)

	e.setStatus(left, "todo")
	e.update(left, map[string]any{"status_changed": true, "prev_status": waitingStatus})

	e.assertArchived("owner items on the issue that left", true, mine...)
	e.assertArchived("another member's item", false, theirs)
	e.assertArchived("an issue still waiting", false, stays)
	st := e.resolver.Stats()
	if st.Archived[ArchiveReasonLeftStatus] != 2 || st.Leaves != 1 || st.Failures != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if len(e.archived) != 1 || e.archived[0]["recipient_id"] != e.user || e.archived[0]["count"] != int64(2) ||
		e.archived[0]["issue_id"] != left {
		t.Fatalf("batch-archived events = %+v", e.archived)
	}
}

func TestInboxResolveListenerIgnoresChangesThatDoNotEndTheWait(t *testing.T) {
	e := newInboxEnv(t)
	moving := e.issue("moving", testutil.Cols{"status": "in_progress"})
	assigned := e.issue("assigned to the owner", testutil.Cols{"status": waitingStatus,
		"assignee_type": "member", "assignee_id": e.user})
	failing := e.issue("failed", testutil.Cols{"status": waitingStatus})
	e.failedTask(e.agentA, failing)
	items := []string{e.item(e.user, moving, 0), e.item(e.user, assigned, 0), e.item(e.user, failing, 0)}

	// Not out of a waiting status.
	e.setStatus(moving, "in_review")
	e.update(moving, map[string]any{"status_changed": true, "prev_status": "in_progress"})
	// Out of the waiting status, but still assigned to the owner.
	e.setStatus(assigned, "todo")
	e.update(assigned, map[string]any{"status_changed": true, "prev_status": waitingStatus})
	// Out of the waiting status, but its latest task failed.
	e.setStatus(failing, "todo")
	e.update(failing, map[string]any{"status_changed": true, "prev_status": waitingStatus})
	// An assignee change away from someone else.
	e.setAssignee(moving, "agent", e.agentA)
	e.update(moving, map[string]any{"assignee_changed": true,
		"prev_assignee_type": strPtr("agent"), "prev_assignee_id": strPtr(e.agentB)})
	// Neither change flagged.
	e.update(moving, nil)

	e.assertArchived("no wait ended", false, items...)
	if st := e.resolver.Stats(); st.Leaves != 2 || st.StillWaiting != 2 || st.Archived[ArchiveReasonLeftStatus] != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestInboxResolveListenerArchivesWhenTheOwnerIsUnassigned(t *testing.T) {
	e := newInboxEnv(t)
	issue := e.issue("handed back", testutil.Cols{"status": "todo", "assignee_type": "member", "assignee_id": e.user})
	item := e.item(e.user, issue, 0)

	e.setAssignee(issue, "agent", e.agentA)
	e.update(issue, map[string]any{"assignee_changed": true,
		"prev_assignee_type": strPtr("member"), "prev_assignee_id": strPtr(e.user)})

	e.assertArchived("unassigned from the owner", true, item)
	if st := e.resolver.Stats(); st.Archived[ArchiveReasonUnassigned] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestInboxResolveDoesNothingWithoutAWorkspaceSetting(t *testing.T) {
	e := newInboxEnv(t)
	if err := SetInboxResolveConfig(e.ctx, pool, e.ws, nil); err != nil {
		t.Fatal(err)
	}
	issue := e.issue("left", testutil.Cols{"status": waitingStatus})
	item := e.item(e.user, issue, time.Hour)
	e.setStatus(issue, "todo")
	e.activity(issue, "status_changed", `{"from":"started_2","to":"todo"}`, 30*time.Minute)

	e.update(issue, map[string]any{"status_changed": true, "prev_status": waitingStatus})
	res, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	e.assertArchived("workspace without a setting", false, item)
	if res.Archived != 0 || res.Workspaces != 0 {
		t.Fatalf("reconcile = %+v", res)
	}
}

func TestInboxResolveListenerFailureOnlyCounts(t *testing.T) {
	e := newInboxEnv(t)
	issue := e.issue("left", testutil.Cols{"status": waitingStatus})
	item := e.item(e.user, issue, 2*time.Minute)
	e.setStatus(issue, "todo")
	e.resolver.Timeout = time.Nanosecond

	e.update(issue, map[string]any{"status_changed": true, "prev_status": waitingStatus})

	if st := e.resolver.Stats(); st.Failures != 1 {
		t.Fatalf("stats = %+v, want one failure", st)
	}
	var status string
	if err := pool.QueryRow(e.ctx, `SELECT status FROM issue WHERE id = $1`, issue).Scan(&status); err != nil || status != "todo" {
		t.Fatalf("issue status = %q (%v)", status, err)
	}
	e.assertArchived("after a failed listener", false, item)

	// Reconcile archives what the listener left.
	e.activity(issue, "status_changed", `{"from":"started_2","to":"todo"}`, time.Minute)
	res, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived != 1 {
		t.Fatalf("reconcile = %+v", res)
	}
	e.assertArchived("after reconcile", true, item)
}

func TestInboxResolveReconcileArchivesWhatTheListenerMissed(t *testing.T) {
	e := newInboxEnv(t)
	left := e.issue("left", testutil.Cols{"status": "todo"})
	unassigned := e.issue("unassigned", testutil.Cols{"status": "in_progress"})
	again := e.issue("waiting again", testutil.Cols{"status": waitingStatus})
	recent := e.issue("left just now", testutil.Cols{"status": "todo"})
	neverWaited := e.issue("never waited", testutil.Cols{"status": "in_review"})

	before := e.item(e.user, left, time.Hour)
	sameChange := e.item(e.user, left, 30*time.Minute-5*time.Second)
	later := e.item(e.user, left, 10*time.Minute)
	e.activity(left, "status_changed", `{"from":"started_2","to":"todo"}`, 30*time.Minute)

	gone := e.item(e.user, unassigned, time.Hour)
	e.activity(unassigned, "assignee_changed",
		fmt.Sprintf(`{"from_type":"member","from_id":%q,"to_type":"agent","to_id":%q}`, e.user, e.agentA), 30*time.Minute)

	back := e.item(e.user, again, time.Hour)
	e.activity(again, "status_changed", `{"from":"started_2","to":"todo"}`, 30*time.Minute)

	racing := e.item(e.user, recent, time.Hour)
	e.activity(recent, "status_changed", `{"from":"started_2","to":"todo"}`, time.Second)

	noise := e.item(e.user, neverWaited, time.Hour)

	res, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	e.assertArchived("written before the issue left the wait", true, before, sameChange, gone)
	e.assertArchived("written after it left", false, later)
	e.assertArchived("waiting again", false, back)
	e.assertArchived("left within the grace, the listener's turn", false, racing)
	e.assertArchived("never waited", false, noise)
	if res.Archived != 3 || res.ArchivedIssues != 2 {
		t.Fatalf("reconcile = %+v", res)
	}
	// later and noise are on issues that do not wait and did not leave a
	// wait after them; racing is excess the next pass archives.
	if res.OutsideWait != 2 {
		t.Fatalf("outside_wait = %d, want 2", res.OutsideWait)
	}
	if st := e.resolver.Stats(); st.Archived[ArchiveReasonReconcile] != 3 || st.ReconcilePasses != 1 {
		t.Fatalf("stats = %+v", st)
	}

	again2, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	if again2.Archived != 0 {
		t.Fatalf("second pass archived %d, want 0", again2.Archived)
	}
}

// TestInboxResolveReconcileOnReplicas: passes running at once archive and
// count each row once.
func TestInboxResolveReconcileOnReplicas(t *testing.T) {
	e := newInboxEnv(t)
	var items []string
	for i := range 5 {
		issue := e.issue(fmt.Sprintf("left %d", i), testutil.Cols{"status": "todo"})
		items = append(items, e.item(e.user, issue, time.Hour), e.item(e.user, issue, time.Hour))
		e.activity(issue, "status_changed", `{"from":"started_2","to":"todo"}`, 30*time.Minute)
	}
	replicas := []*InboxResolver{NewInboxResolver(pool, nil), NewInboxResolver(pool, nil), NewInboxResolver(pool, nil)}
	var wg sync.WaitGroup
	totals := make([]int64, len(replicas))
	for i, r := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.ReconcileWorkspace(context.Background(), e.ws)
			if err != nil {
				t.Error(err)
			}
			totals[i] = res.Archived
		}()
	}
	wg.Wait()
	var sum int64
	for _, n := range totals {
		sum += n
	}
	if sum != int64(len(items)) {
		t.Fatalf("replicas archived %v, want %d in total", totals, len(items))
	}
	e.assertArchived("after concurrent passes", true, items...)
}

func TestInboxResolveCountsMissingItemsWithoutWritingThem(t *testing.T) {
	e := newInboxEnv(t)
	e.issue("waiting, no item", testutil.Cols{"status": waitingStatus})
	e.issue("assigned, no item", testutil.Cols{"status": "todo", "assignee_type": "member", "assignee_id": e.user})
	failed := e.issue("failed, no item", testutil.Cols{"status": "in_progress"})
	e.failedTask(e.agentA, failed)
	closed := e.issue("assigned but done", testutil.Cols{"status": "done", "assignee_type": "member", "assignee_id": e.user})
	e.failedTask(e.agentA, closed)
	retried := e.issue("failed then retried", testutil.Cols{"status": "in_progress"})
	first := e.failedTask(e.agentA, retried, testutil.Cols{"created_at": testutil.Raw("now() - interval '1 hour'")})
	e.fx.Task(t, e.agentA, testutil.Cols{"issue_id": retried, "status": "queued", "retry_of_task_id": first, "runtime_id": e.runtimeOf(e.agentA)})
	covered := e.issue("waiting, has an item", testutil.Cols{"status": waitingStatus})
	e.item(e.user, covered, 0)

	res, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{MissingReasonWaitingStatus: 1, MissingReasonAssigned: 1, MissingReasonFailed: 1}
	if fmt.Sprint(res.Missing) != fmt.Sprint(want) {
		t.Fatalf("missing = %v, want %v", res.Missing, want)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1`, e.ws); n != 1 {
		t.Fatalf("inbox items = %d, want the one the test wrote", n)
	}
	survey, err := SurveyInbox(e.ctx, pool, e.ws, InboxResolveConfig{OwnerUserID: e.user, WaitingStatuses: []string{waitingStatus}})
	if err != nil || fmt.Sprint(survey.Missing) != fmt.Sprint(want) || survey.Excess != 0 {
		t.Fatalf("survey = %+v (%v)", survey, err)
	}
}

func TestInboxResolveConfigCacheFollowsChanges(t *testing.T) {
	e := newInboxEnv(t)
	issue := e.issue("left", testutil.Cols{"status": "review_owner"})
	item := e.item(e.user, issue, 0)
	// Cached with started_2 as the waiting status.
	e.setStatus(issue, "todo")
	e.update(issue, map[string]any{"status_changed": true, "prev_status": "review_owner"})
	e.assertArchived("not a waiting status yet", false, item)

	cfg := InboxResolveConfig{OwnerUserID: e.user, WaitingStatuses: []string{waitingStatus, "review_owner"}}
	if err := SetInboxResolveConfig(e.ctx, pool, e.ws, &cfg); err != nil {
		t.Fatal(err)
	}
	e.update(issue, map[string]any{"status_changed": true, "prev_status": "review_owner"})
	e.assertArchived("after the setting changed", true, item)
}

// TestRegisterInboxResolveFollowsTheFlag: without router_inbox_resolve the
// inbox listener and its job are not registered.
func TestRegisterInboxResolveFollowsTheFlag(t *testing.T) {
	bus := events.New()
	jobs := &recordingRegistrar{}
	v1 := featureflag.NewStaticProvider()
	v1.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	Register(context.Background(), featureflag.NewService(v1), bus, nil, jobs, nil)
	if fmt.Sprint(jobs.names) != "["+JobNameReconcile+"]" {
		t.Fatalf("jobs = %v", jobs.names)
	}

	bus, jobs = events.New(), &recordingRegistrar{}
	both := featureflag.NewStaticProvider()
	both.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	both.Set(FlagInboxResolve, featureflag.Rule{Default: true})
	Register(context.Background(), featureflag.NewService(both), bus, nil, jobs, nil)
	if fmt.Sprint(jobs.names) != "["+JobNameReconcile+" "+JobNameInboxResolve+"]" {
		t.Fatalf("jobs = %v", jobs.names)
	}
	if n := bus.SubscriberCount(protocol.EventIssueUpdated); n != 2 {
		t.Fatalf("issue:updated listeners = %d, want capture and inbox resolve", n)
	}
	t.Cleanup(func() { activeInboxResolver.Store(nil); SetActivityAnnotation(false) })
	if !activityAnnotation.Load() {
		t.Fatal("handoff annotation not turned on with inbox resolve")
	}

	bus, jobs = events.New(), &recordingRegistrar{}
	only := featureflag.NewStaticProvider()
	only.Set(FlagInboxResolve, featureflag.Rule{Default: true})
	if Register(context.Background(), featureflag.NewService(only), bus, nil, jobs, nil) || len(jobs.names) != 0 ||
		bus.SubscriberCount(protocol.EventIssueUpdated) != 0 {
		t.Fatalf("registered without router_v1: jobs = %v", jobs.names)
	}
}

// handoff records a workflow handoff the way the upstream activity listener
// writes it: one status row carrying the new assignee as handoff_*, and no
// assignee row.
func (e *inboxEnv) handoff(issueID, from, to, toType, toID string, ago time.Duration) {
	e.t.Helper()
	e.activity(issueID, "status_changed", fmt.Sprintf(
		`{"from":%q,"to":%q,"handoff_workflow":"Delivery","handoff_to_type":%q,"handoff_to_id":%q,"handoff_run":"true"}`,
		from, to, toType, toID), ago)
}

// TestInboxResolveReconcileFollowsWorkflowHandoffs: a handoff that takes an
// issue away from the owner outside a waiting status is a leave, though its
// activity row does not name the previous assignee.
func TestInboxResolveReconcileFollowsWorkflowHandoffs(t *testing.T) {
	e := newInboxEnv(t)
	afterOwner := func(title string) string {
		return e.issue(title, testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": e.agentA})
	}

	// Assigned to the owner by an assignee change, then handed off.
	chained := afterOwner("assigned, then handed off")
	e.activity(chained, "assignee_changed", fmt.Sprintf(`{"to_type":"member","to_id":%q}`, e.user), 2*time.Hour)
	chainedItem := e.item(e.user, chained, time.Hour)
	e.handoff(chained, "todo", "in_progress", "agent", e.agentA, 30*time.Minute)

	// Assigned to the owner by an earlier handoff, then handed off again.
	handedTwice := afterOwner("handed to the owner, then away")
	e.handoff(handedTwice, "backlog", "todo", "member", e.user, 2*time.Hour)
	handedTwiceItem := e.item(e.user, handedTwice, time.Hour)
	e.handoff(handedTwice, "todo", "in_progress", "agent", e.agentA, 30*time.Minute)

	// Created assigned to the owner (the issue_assigned item shows it), then
	// handed off before any other assignment.
	created := afterOwner("created for the owner")
	assignedItem := e.fx.Insert(t, "inbox_item", testutil.Cols{
		"workspace_id": e.ws, "recipient_type": "member", "recipient_id": e.user, "type": "issue_assigned",
		"severity": "action_required", "title": "assigned", "issue_id": created,
		"created_at": testutil.Raw("now() - interval '2 hours'"),
	})
	createdItem := e.item(e.user, created, time.Hour)
	e.handoff(created, "todo", "in_progress", "agent", e.agentA, 30*time.Minute)

	// Someone else's issue handed between agents: the owner's mention stays.
	others := afterOwner("between agents")
	e.activity(others, "assignee_changed", fmt.Sprintf(`{"to_type":"agent","to_id":%q}`, e.agentB), 2*time.Hour)
	othersItem := e.item(e.user, others, time.Hour)
	e.handoff(others, "todo", "in_progress", "agent", e.agentA, 30*time.Minute)

	// Unassigned before the handoff: the owner was no longer the assignee.
	dropped := afterOwner("owner dropped earlier")
	e.activity(dropped, "assignee_changed", fmt.Sprintf(`{"to_type":"member","to_id":%q}`, e.user), 3*time.Hour)
	droppedItem := e.item(e.user, dropped, 10*time.Minute)
	e.activity(dropped, "assignee_changed", fmt.Sprintf(`{"from_type":"member","from_id":%q}`, e.user), 2*time.Hour)
	e.handoff(dropped, "todo", "in_progress", "agent", e.agentA, 5*time.Minute)

	// Created assigned to the owner by the owner (no issue_assigned item),
	// first handed off with the previous assignee recorded
	// (AnnotateHandoffActivity); and the same from someone else or from no one.
	annotated := func(title, fromType, fromID string) string {
		issue := afterOwner(title)
		item := e.item(e.user, issue, time.Hour)
		e.activity(issue, "status_changed", fmt.Sprintf(
			`{"from":"todo","to":"in_progress","handoff_to_type":"agent","handoff_to_id":%q,"handoff_from_type":%q,"handoff_from_id":%q}`,
			e.agentA, fromType, fromID), 30*time.Minute)
		return item
	}
	selfAssignedItem := annotated("self-assigned, handed off", "member", e.user)
	fromOtherItem := annotated("someone else's, handed off", "agent", e.agentB)
	fromNoneItem := annotated("unassigned, handed off", "", "")

	res, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	e.assertArchived("handed off away from the owner", true, chainedItem, handedTwiceItem, assignedItem, createdItem, selfAssignedItem)
	e.assertArchived("never the owner's", false, othersItem, droppedItem, fromOtherItem, fromNoneItem)
	if res.Archived != 5 || res.ArchivedIssues != 4 {
		t.Fatalf("reconcile = %+v", res)
	}
}

func TestInboxResolveListenerArchivesWhenTheOwnersIssueCloses(t *testing.T) {
	e := newInboxEnv(t)
	closed := e.issue("closed", testutil.Cols{"status": "todo", "assignee_type": "member", "assignee_id": e.user})
	moved := e.issue("moved on", testutil.Cols{"status": "todo", "assignee_type": "member", "assignee_id": e.user})
	closedItem, movedItem := e.item(e.user, closed, 0), e.item(e.user, moved, 0)

	e.setStatus(closed, "done")
	e.update(closed, map[string]any{"status_changed": true, "prev_status": "todo"})
	e.setStatus(moved, "in_progress")
	e.update(moved, map[string]any{"status_changed": true, "prev_status": "todo"})

	e.assertArchived("the owner's issue closed", true, closedItem)
	e.assertArchived("the owner's issue still open", false, movedItem)
	if st := e.resolver.Stats(); st.Archived[ArchiveReasonLeftStatus] != 1 || st.StillWaiting != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestInboxResolveReconcileFollowsClosingAndDuplicates: closing an issue
// assigned to the owner, and a duplicate mark, whose activity row replaces the
// status row and names no previous status, both end the wait.
func TestInboxResolveReconcileFollowsClosingAndDuplicates(t *testing.T) {
	e := newInboxEnv(t)
	closed := e.issue("closed", testutil.Cols{"status": "done", "assignee_type": "member", "assignee_id": e.user})
	closedItem := e.item(e.user, closed, time.Hour)
	e.activity(closed, "status_changed", `{"from":"todo","to":"done"}`, 30*time.Minute)

	dup := e.issue("duplicate", testutil.Cols{"status": "cancelled"})
	e.activity(dup, "status_changed", `{"from":"todo","to":"started_2"}`, 2*time.Hour)
	dupItem := e.item(e.user, dup, time.Hour)
	e.activity(dup, "duplicate_marked", `{"original_id":"x","original_identifier":"RT-1"}`, 30*time.Minute)

	reopened := e.issue("closed, then the owner's", testutil.Cols{"status": "done", "assignee_type": "member", "assignee_id": e.user})
	e.activity(reopened, "status_changed", `{"from":"in_progress","to":"done"}`, 2*time.Hour)
	reopenedItem := e.item(e.user, reopened, time.Hour)
	e.activity(reopened, "title_changed", `{}`, 30*time.Minute)

	if _, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	e.assertArchived("waited after the item, now closed", true, closedItem, dupItem)
	e.assertArchived("closed before the item", false, reopenedItem)
}

// TestInboxResolveFollowsBatchAssigneeChanges: a batch update carries the
// previous assignee under the fork's keys and its assignee row under
// router_from_*; both the listener and reconcile read them.
func TestInboxResolveFollowsBatchAssigneeChanges(t *testing.T) {
	e := newInboxEnv(t)
	member := "member"

	viaListener := e.issue("batch reassigned", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": e.agentA})
	listenerItem := e.item(e.user, viaListener, 0)
	e.update(viaListener, map[string]any{"assignee_changed": true,
		PayloadPrevAssigneeType: &member, PayloadPrevAssigneeID: &e.user})
	e.assertArchived("batch payload names the owner", true, listenerItem)

	viaReconcile := e.issue("batch reassigned, listener missed", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": e.agentA})
	reconcileItem := e.item(e.user, viaReconcile, time.Hour)
	e.activity(viaReconcile, "assignee_changed", fmt.Sprintf(
		`{"to_type":"agent","to_id":%q,"router_from_type":"member","router_from_id":%q}`, e.agentA, e.user), 30*time.Minute)

	fromNone := e.issue("batch assigned from no one", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": e.agentA})
	fromNoneItem := e.item(e.user, fromNone, time.Hour)
	e.activity(fromNone, "assignee_changed", fmt.Sprintf(
		`{"to_type":"agent","to_id":%q,"router_from_type":"","router_from_id":""}`, e.agentA), 30*time.Minute)

	if _, err := e.resolver.ReconcileWorkspace(e.ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	e.assertArchived("assignee row names the owner", true, reconcileItem)
	e.assertArchived("assignee row names no one", false, fromNoneItem)
}
