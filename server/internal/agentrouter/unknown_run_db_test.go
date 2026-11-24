// Added by the mouse9527/multica fork: only the Super Agent's own runs are its
// own; an unknown run is someone else's, and the self-loop brake (M-233).

package agentrouter

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// proposalMeta is the metadata the Evolution proposal store stamps on a
// proposal issue.
var proposalMeta = testutil.Raw(`'{"evolution_fingerprint":"fp-m233"}'::jsonb`)

// publishAgentComment sends a comment event by agent A, the way the server
// publishes a comment it wrote in the agent's name.
func (e *env) publishAgentComment(commentID string) {
	e.capt.Handle(events.Event{Type: protocol.EventCommentCreated, WorkspaceID: e.ws, ActorType: "agent", ActorID: e.agentA,
		Payload: map[string]any{"comment": map[string]any{"id": commentID}}})
}

// TestOnlyTheSuperAgentsRunsAreItsOwn: the listener marks an action of the
// subscribed agent as its own only when it came from the Super Agent's chat
// or a run dispatch started for it. The server writing in the agent's name
// (an issue or comment with no run), an autopilot's issue, an action whose
// run is unknown and the same agent in another chat all reach the batch.
func TestOnlyTheSuperAgentsRunsAreItsOwn(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()

	want := map[string]bool{}
	names := map[string]string{}
	created := func(name, task string, cols testutil.Cols, self bool) {
		c := testutil.Cols{"parent_issue_id": r.child, "creator_type": "agent", "creator_id": e.agentA}
		for k, v := range cols {
			c[k] = v
		}
		id := e.issue(name, c)
		e.publishIssueFrom(protocol.EventIssueCreated, id, task)
		want[id], names[id] = self, name
	}
	created("issue the server created in the agent's name", "", nil, false)
	created("proposal the server filed", "", testutil.Cols{"metadata": proposalMeta}, false)
	created("autopilot's issue", "", testutil.Cols{"origin_type": "autopilot", "origin_id": r.issueTask}, false)
	created("issue from another chat", r.otherChatTurn, nil, false)
	created("issue from the chat turn", r.chatTurn, nil, true)
	created("issue agent-created from the chat turn, run on the origin only", "",
		testutil.Cols{"origin_type": "agent_create", "origin_id": r.chatTurn}, true)
	created("issue from a dispatched run", r.dispatched, nil, true)

	comment := func(name, task string, self bool) {
		id := e.agentComment(r.child, task)
		e.publishAgentComment(id)
		want[id], names[id] = self, name
	}
	comment("comment the server wrote in the agent's name", "", false)
	comment("comment from another chat", r.otherChatTurn, false)
	comment("comment from the chat turn", r.chatTurn, true)
	comment("comment from a retry of a dispatched run", r.retry, true)

	assertSelf(t, selfByKey(e.events(r.sub.ID)), want, names)
}

// TestReconcileTreatsUnknownRunsAsOthers: reconcile, from the rows alone,
// reaches the same verdicts.
func TestReconcileTreatsUnknownRunsAsOthers(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()

	byAgent := func(name string, cols testutil.Cols) string {
		c := testutil.Cols{"parent_issue_id": r.child, "creator_type": "agent", "creator_id": e.agentA}
		for k, v := range cols {
			c[k] = v
		}
		return e.issue(name, c)
	}
	serverIssue := byAgent("issue the server created", nil)
	proposal := byAgent("proposal the server filed", testutil.Cols{"metadata": proposalMeta})
	fromChat := byAgent("issue agent-created from the chat turn",
		testutil.Cols{"origin_type": "agent_create", "origin_id": r.chatTurn})
	fromOtherChat := byAgent("issue quick-created from another chat",
		testutil.Cols{"origin_type": "quick_create", "origin_id": r.otherChatTurn})
	serverComment := e.agentComment(proposal, "")
	chatComment := e.agentComment(r.child, r.chatTurn)
	otherChatComment := e.agentComment(r.child, r.otherChatTurn)
	e.reconcile()

	assertSelf(t, selfByKey(e.events(r.sub.ID)), map[string]bool{
		serverIssue: false, proposal: false, fromChat: true, fromOtherChat: false,
		serverComment: false, chatComment: true, otherChatComment: false,
	}, map[string]string{
		serverIssue: "issue the server created", proposal: "proposal the server filed",
		fromChat: "issue from the chat turn", fromOtherChat: "issue from another chat",
		serverComment: "comment the server wrote", chatComment: "comment from the chat turn",
		otherChatComment: "comment from another chat",
	})
}

// TestSuperAgentChatWritesNeverWakeIt is the regression the new rule must
// keep: everything a Super Agent's chat turn writes records the turn, so none
// of it starts a batch. A proposal the server files in the agent's name, in
// the project the Super Agent watches, starts one.
func TestSuperAgentChatWritesNeverWakeIt(t *testing.T) {
	e, d, calls := newDispatchEnv(t)
	d.Chat = true
	project := e.fx.Project(t, "evolution output")
	// Issues created before the subscription are its baseline, not events.
	board := e.issue("board", testutil.Cols{"project_id": project})
	work := e.issue("work", testutil.Cols{"project_id": project})
	sub := e.subscribe(e.agentA, ScopeProject, project)
	chat := e.fx.ChatSession(t, e.agentA)
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{
		AnchorIssueID: &board, AnchorChatSessionID: &chat, Change: testChange,
	}); err != nil {
		t.Fatalf("bind the project scope: %v", err)
	}
	turn := e.fx.Task(t, e.agentA, testutil.Cols{
		"chat_session_id": chat, "runtime_id": e.runtimeOf(e.agentA),
		"status": "running", "started_at": testutil.Raw("now()"),
	})

	// Comment, status change, reassignment and a new issue, each the way the
	// API records a chat turn's write.
	e.publishComment(e.comment(work, testutil.Cols{"author_type": "agent", "author_id": e.agentA, "source_task_id": turn}))
	e.setStatus(work, "in_progress")
	e.publishIssueFrom(protocol.EventIssueUpdated, work, turn)
	e.setAssignee(work, "agent", e.agentB)
	e.publishIssueFrom(protocol.EventIssueUpdated, work, turn)
	created := e.issue("filed from the chat", testutil.Cols{
		"project_id": project, "creator_type": "agent", "creator_id": e.agentA,
		"origin_type": "agent_create", "origin_id": turn,
	})
	e.publishIssueFrom(protocol.EventIssueCreated, created, turn)
	e.age(sub.ID)
	e.tick(d)
	e.reconcile()
	e.age(sub.ID)
	e.tick(d)
	if len(calls.calls) != 0 {
		t.Fatalf("the Super Agent's own chat writes started %d run(s)", len(calls.calls))
	}
	for _, ev := range e.pending(sub.ID) {
		if !ev.Self {
			t.Fatalf("a chat write was not its own: %+v", ev)
		}
	}

	proposal := e.issue("proposal", testutil.Cols{
		"project_id": project, "creator_type": "agent", "creator_id": e.agentA, "metadata": proposalMeta,
	})
	e.publishIssueFrom(protocol.EventIssueCreated, proposal, "")
	e.age(sub.ID)
	e.tick(d)
	if len(calls.calls) != 1 {
		t.Fatalf("runs = %d, want 1 for the proposal the server filed", len(calls.calls))
	}
	if c := calls.calls[0]; c.chat != chat || !strings.Contains(c.note, "1 new event(s)") || !strings.Contains(c.note, KindIssueCreated) {
		t.Fatalf("batch is not the proposal alone in the Super Agent's chat: chat %s\n%s", c.chat, c.note)
	}
}

// TestSelfLoopBrake: after selfLoopStreak runs in a row started only by the
// subscribed agent's events, another such batch waits for the cooldown and
// the owner is told once; an event from anyone else goes at once and takes
// the waiting events along.
func TestSelfLoopBrake(t *testing.T) {
	e, d, calls := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	child := e.issue("router child", testutil.Cols{"parent_issue_id": root})
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)

	// An agent comment with no run: what a Super Agent write that lost its
	// run id would look like.
	agentOnly := func() {
		t.Helper()
		e.publishAgentComment(e.agentComment(child, ""))
		e.age(sub.ID)
		e.tick(d)
	}
	finishLast := func() {
		t.Helper()
		e.setTask(calls.calls[len(calls.calls)-1].task, "completed")
	}
	for i := range selfLoopStreak {
		agentOnly()
		if len(calls.calls) != i+1 {
			t.Fatalf("run %d: runs = %d, want %d before the brake", i+1, len(calls.calls), i+1)
		}
		finishLast()
	}
	selfLoopNotices := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM owner_notice
			WHERE source_subscription_id = $1 AND dedupe_key LIKE 'router_self_loop:%'`, sub.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	agentOnly()
	if len(calls.calls) != selfLoopStreak {
		t.Fatalf("runs = %d, want %d: the brake let an agent-only batch through", len(calls.calls), selfLoopStreak)
	}
	if p := e.pending(sub.ID); len(p) != 1 {
		t.Fatalf("pending = %d, want the held event", len(p))
	}
	e.tick(d)
	if n := selfLoopNotices(); n != 1 {
		t.Fatalf("self-loop notices = %d, want 1", n)
	}

	// The cooldown passes: the held batch goes.
	e.fx.Exec(t, `UPDATE router_dispatch SET created_at = created_at - interval '16 minutes' WHERE subscription_id = $1`, sub.ID)
	e.tick(d)
	if len(calls.calls) != selfLoopStreak+1 {
		t.Fatalf("runs = %d after the cooldown, want %d", len(calls.calls), selfLoopStreak+1)
	}
	finishLast()

	// Still in a streak: the next agent-only batch waits again, and a member's
	// comment takes it along at once.
	agentOnly()
	if len(calls.calls) != selfLoopStreak+1 {
		t.Fatalf("runs = %d, want the agent-only batch held again", len(calls.calls))
	}
	e.publishComment(e.comment(child))
	e.age(sub.ID)
	e.tick(d)
	if len(calls.calls) != selfLoopStreak+2 {
		t.Fatalf("runs = %d, want the member's comment to start one", len(calls.calls))
	}
	if note := calls.calls[len(calls.calls)-1].note; !strings.Contains(note, "2 new event(s)") {
		t.Fatalf("batch does not carry the held event with the member's:\n%s", note)
	}
	finishLast()

	// The member's batch broke the streak.
	agentOnly()
	if len(calls.calls) != selfLoopStreak+3 {
		t.Fatalf("runs = %d, want an agent-only batch to go once the streak is broken", len(calls.calls))
	}
}
