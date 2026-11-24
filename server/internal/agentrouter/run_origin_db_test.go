// Added by the mouse9527/multica fork: own actions by run (M-193); an unknown
// run is someone else's (M-233).

package agentrouter

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// runFixture is one Super Agent (agent A, chat-bound, on the tree under root)
// and the runs its agent acts from.
type runFixture struct {
	sub         Subscription
	root, child string
	// The Super Agent's own runs.
	chatTurn, dispatched, retry string
	// The same agent's other work.
	issueTask, otherChatTurn, onAnchor string
}

func (e *env) superAgentRuns() runFixture {
	e.t.Helper()
	var r runFixture
	r.root = e.issue("root")
	r.child = e.issue("child", testutil.Cols{"parent_issue_id": r.root})
	r.sub = e.subscribe(e.agentA, ScopeIssueTree, r.root)
	chat := e.bindChat(r.sub)
	// Running, not queued: one issue may hold several of these runs, and the
	// pending-task unique index allows only one queued run per issue and agent.
	task := func(cols testutil.Cols) string {
		cols["runtime_id"] = e.runtimeOf(e.agentA)
		cols["status"] = "running"
		cols["started_at"] = testutil.Raw("now()")
		return e.fx.Task(e.t, e.agentA, cols)
	}
	r.chatTurn = task(testutil.Cols{"chat_session_id": chat})
	r.dispatched = task(testutil.Cols{"issue_id": r.root})
	e.fx.Insert(e.t, "router_dispatch", testutil.Cols{
		"workspace_id": e.ws, "subscription_id": r.sub.ID, "agent_id": e.agentA,
		"anchor_issue_id": r.root, "task_id": r.dispatched, "event_count": 1,
		"outcome": "completed", "settled_at": testutil.Raw("now()"),
	})
	r.retry = task(testutil.Cols{"issue_id": r.root, "parent_task_id": r.dispatched, "retry_of_task_id": r.dispatched})
	r.issueTask = task(testutil.Cols{"issue_id": r.child})
	r.otherChatTurn = task(testutil.Cols{"chat_session_id": e.fx.ChatSession(e.t, e.agentA)})
	// The tree root is the anchor issue; agent A working it as an ordinary
	// issue task is not the Super Agent.
	r.onAnchor = task(testutil.Cols{"issue_id": r.root})
	return r
}

// ownRun is a finished run dispatch started for sub on issueID: an action
// from it is the Super Agent's own.
func (e *env) ownRun(sub Subscription, issueID string) string {
	e.t.Helper()
	task := e.fx.Task(e.t, sub.AgentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": e.runtimeOf(sub.AgentID), "status": "completed",
		"started_at": testutil.Raw("now()"), "completed_at": testutil.Raw("now()"),
	})
	e.fx.Insert(e.t, "router_dispatch", testutil.Cols{
		"workspace_id": e.ws, "subscription_id": sub.ID, "agent_id": sub.AgentID,
		"anchor_issue_id": issueID, "task_id": task, "event_count": 1,
		"outcome": "completed", "settled_at": testutil.Raw("now()"),
	})
	return task
}

// ownComment writes a comment by agent A from one of sub's own runs.
func (e *env) ownComment(sub Subscription, issueID string, run string) string {
	e.t.Helper()
	return e.comment(issueID, testutil.Cols{"author_type": "agent", "author_id": e.agentA, "source_task_id": run})
}

// agentComment writes a comment by agent A from task ("" for none).
func (e *env) agentComment(issueID, task string) string {
	e.t.Helper()
	cols := testutil.Cols{"author_type": "agent", "author_id": e.agentA}
	if task != "" {
		cols["source_task_id"] = task
	}
	return e.comment(issueID, cols)
}

func (e *env) publishIssueFrom(eventType, issueID, task string) {
	e.t.Helper()
	e.capt.Handle(events.Event{Type: eventType, WorkspaceID: e.ws, ActorType: "agent", ActorID: e.agentA,
		ActorTaskID: task, Payload: map[string]any{"issue": e.issuePayload(issueID)}})
}

// selfByKey maps each event's comment id, status "to", created issue id or
// kind to is_self.
func selfByKey(evs []Event) map[string]bool {
	out := map[string]bool{}
	for _, ev := range evs {
		switch ev.Kind {
		case KindCommentCreated:
			out[ev.Payload["comment_id"].(string)] = ev.Self
		case KindStatusChanged:
			out["status:"+ev.Payload["to"].(string)] = ev.Self
		case KindIssueCreated:
			out[ev.IssueID] = ev.Self
		default:
			out[ev.Kind] = ev.Self
		}
	}
	return out
}

func assertSelf(t *testing.T, got map[string]bool, want map[string]bool, names map[string]string) {
	t.Helper()
	for key, self := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("%s: no event", names[key])
			continue
		}
		if g != self {
			t.Errorf("%s: self = %v, want %v", names[key], g, self)
		}
	}
}

// TestOwnActionsAreTheSuperAgentsRunsOnly: the listener marks an action of the
// subscribed agent as its own only when it came from the Super Agent's chat
// or a run dispatch started; the same agent's issue tasks and other chats
// count as anyone else, and so does an action whose run is unknown (M-233).
func TestOwnActionsAreTheSuperAgentsRunsOnly(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()

	want := map[string]bool{}
	names := map[string]string{}
	comment := func(name, task string, self bool) {
		id := e.agentComment(r.child, task)
		e.publishComment(id)
		want[id], names[id] = self, "comment from "+name
	}
	comment("the chat turn", r.chatTurn, true)
	comment("a dispatched run", r.dispatched, true)
	comment("a retry of a dispatched run", r.retry, true)
	comment("no known run", "", false)
	comment("an issue task", r.issueTask, false)
	comment("another chat", r.otherChatTurn, false)
	comment("an issue task on the anchor", r.onAnchor, false)

	status := func(to, task string, self bool) {
		e.setStatus(r.child, to)
		e.publishIssueFrom(protocol.EventIssueUpdated, r.child, task)
		want["status:"+to], names["status:"+to] = self, "status change to "+to
	}
	status("in_progress", r.issueTask, false)
	status("in_review", r.chatTurn, true)
	status("done", "", false)

	created := func(name, eventTask string, origin testutil.Cols, self bool) {
		cols := testutil.Cols{"parent_issue_id": r.child, "creator_type": "agent", "creator_id": e.agentA}
		for k, v := range origin {
			cols[k] = v
		}
		id := e.issue(name, cols)
		e.publishIssueFrom(protocol.EventIssueCreated, id, eventTask)
		want[id], names[id] = self, name
	}
	created("issue created from an issue task", r.issueTask, nil, false)
	created("issue created from the chat turn", r.chatTurn, nil, true)
	// An event that does not name its run falls back to the issue's origin.
	created("quick-created issue from an issue task", "",
		testutil.Cols{"origin_type": "quick_create", "origin_id": r.issueTask}, false)

	failed := e.failedTask(e.agentA, r.child, testutil.Cols{"runtime_id": e.runtimeOf(e.agentA)})
	e.publishTaskFailed(failed, r.child, e.agentA, false)
	want[KindTaskFailed], names[KindTaskFailed] = false, "failed issue task"

	assertSelf(t, selfByKey(e.events(r.sub.ID)), want, names)
}

// TestReconcileJudgesOwnActionsByRun: reconcile, which reads the rows the
// listener missed, reaches the same verdicts from what the rows record.
func TestReconcileJudgesOwnActionsByRun(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()

	fromIssueTask := e.agentComment(r.child, r.issueTask)
	fromChat := e.agentComment(r.child, r.chatTurn)
	unknown := e.agentComment(r.child, "")
	e.failedTask(e.agentA, r.child, testutil.Cols{"runtime_id": e.runtimeOf(e.agentA)})
	// An issue records the run that created it in origin_id for agent_create
	// and quick_create; an autopilot's origin_id is not a task.
	createdBy := func(originType, originID string) string {
		return e.issue("sub-issue "+originType, testutil.Cols{"parent_issue_id": r.child,
			"creator_type": "agent", "creator_id": e.agentA, "origin_type": originType, "origin_id": originID})
	}
	createdByExecutor := createdBy("agent_create", r.issueTask)
	createdByChat := createdBy("quick_create", r.chatTurn)
	createdByAutopilot := createdBy("autopilot", r.issueTask)
	e.reconcile()

	assertSelf(t, selfByKey(e.events(r.sub.ID)), map[string]bool{
		fromIssueTask: false, fromChat: true, unknown: false, KindTaskFailed: false,
		createdByExecutor: false, createdByChat: true, createdByAutopilot: false,
	}, map[string]string{
		fromIssueTask: "comment from an issue task", fromChat: "comment from the chat turn",
		unknown: "comment from no known run", KindTaskFailed: "failed issue task",
		createdByExecutor: "issue created from an issue task", createdByChat: "issue quick-created from the chat turn",
		createdByAutopilot: "issue created by an autopilot, run unknown",
	})
}

// TestWorkHandedToTheSameAgentStartsABatch is the reported case: the Super
// Agent hands work to its own agent, and that work's progress reaches its
// chat, while its own turns still never start one.
func TestWorkHandedToTheSameAgentStartsABatch(t *testing.T) {
	e, d, calls := newDispatchEnv(t)
	d.Chat = true
	r := e.superAgentRuns()

	e.publishComment(e.agentComment(r.child, r.chatTurn))
	e.age(r.sub.ID)
	e.tick(d)
	if len(calls.calls) != 0 {
		t.Fatalf("the Super Agent's own comment started %d run(s)", len(calls.calls))
	}

	e.setStatus(r.child, "in_review")
	e.publishIssueFrom(protocol.EventIssueUpdated, r.child, r.issueTask)
	e.age(r.sub.ID)
	e.tick(d)
	if len(calls.calls) != 1 {
		t.Fatalf("runs = %d, want 1 for the executor's status change", len(calls.calls))
	}
	if note := calls.calls[0].note; !strings.Contains(note, "in_review") || !strings.Contains(note, "1 new event(s)") {
		t.Fatalf("batch does not show the executor's change alone:\n%s", note)
	}
}

// TestShadowHoldTakesTheSameAgentsIssueWork: an inbox item caused by a comment
// of the subscribed agent is the Super Agent's own only when the comment came
// from its own run; an item with no comment has no known run and is taken
// (M-233).
func TestShadowHoldTakesTheSameAgentsIssueWork(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()
	e.shadowHold()

	fromChat := e.agentComment(r.child, r.chatTurn)
	fromIssueTask := e.agentComment(r.child, r.issueTask)
	e.writeInbox(inboxWrite{recipient: e.user, issue: r.child, typ: "new_comment", actorType: "agent", actorID: e.agentA,
		details: `{"comment_id":"` + fromChat + `"}`})
	taken := e.writeInbox(inboxWrite{recipient: e.user, issue: r.child, typ: "new_comment", actorType: "agent", actorID: e.agentA,
		details: `{"comment_id":"` + fromIssueTask + `"}`})
	// Without a comment the run is unknown: someone else's (M-233).
	unknown := e.writeInbox(inboxWrite{recipient: e.user, issue: r.child, typ: "status_changed", actorType: "agent", actorID: e.agentA})

	got := map[string]bool{}
	for _, h := range e.holds() {
		got[h.InboxItemID] = true
	}
	if len(got) != 2 || !got[taken] || !got[unknown] {
		t.Fatalf("holds = %v, want the items from the issue task's comment and with no known run", got)
	}
}

func TestTaskChatSession(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()
	var chat string
	if err := pool.QueryRow(e.ctx, `SELECT chat_session_id::text FROM router_chat_anchor WHERE subscription_id = $1`, r.sub.ID).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	for task, want := range map[string]string{r.chatTurn: chat, r.issueTask: "", "00000000-0000-0000-0000-000000000000": ""} {
		got, err := TaskChatSession(e.ctx, pool, task)
		if err != nil || got != want {
			t.Errorf("TaskChatSession(%s) = %q, %v; want %q", task, got, err, want)
		}
	}
}
