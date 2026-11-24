// Added by the mouse9527/multica fork: the server's writes in an agent's name
// are not the Super Agent's own (M-233).

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

// publishAgentIssueCreated sends the created event for an issue agent A
// created, as IssueService does: with the run it acted from, or marked as the
// server's write.
func (e *env) publishAgentIssueCreated(issueID, task string, serverWrite bool) {
	e.t.Helper()
	e.capt.Handle(events.Event{Type: protocol.EventIssueCreated, WorkspaceID: e.ws, ActorType: "agent", ActorID: e.agentA,
		ActorTaskID: task, ServerWrite: serverWrite, Payload: map[string]any{"issue": e.issuePayload(issueID)}})
}

// publishAgentComment sends a comment event by agent A, as evolution's
// PublishComment does when serverWrite is set.
func (e *env) publishAgentComment(commentID string, serverWrite bool) {
	e.capt.Handle(events.Event{Type: protocol.EventCommentCreated, WorkspaceID: e.ws, ActorType: "agent", ActorID: e.agentA,
		ServerWrite: serverWrite, Payload: map[string]any{"comment": map[string]any{"id": commentID}}})
}

// TestServerWritesAreNotTheSuperAgentsOwn: an issue or comment the server
// wrote in the subscribed agent's name, with no run, reaches the batch like
// anyone else's. Without the mark an action with no known run stays its own,
// and the mark does not override a run that is known.
func TestServerWritesAreNotTheSuperAgentsOwn(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()

	want := map[string]bool{}
	names := map[string]string{}
	created := func(name, task string, serverWrite bool, origin testutil.Cols, self bool) {
		cols := testutil.Cols{"parent_issue_id": r.child, "creator_type": "agent", "creator_id": e.agentA}
		for k, v := range origin {
			cols[k] = v
		}
		id := e.issue(name, cols)
		e.publishAgentIssueCreated(id, task, serverWrite)
		want[id], names[id] = self, name
	}
	created("issue the server created", "", true, nil, false)
	created("issue with no known run", "", false, nil, true)
	created("marked issue from the chat turn", r.chatTurn, true, nil, true)
	created("marked issue quick-created from the chat turn", "", true,
		testutil.Cols{"origin_type": "quick_create", "origin_id": r.chatTurn}, true)

	comment := func(name, task string, serverWrite bool, self bool) {
		id := e.agentComment(r.child, task)
		e.publishAgentComment(id, serverWrite)
		want[id], names[id] = self, name
	}
	comment("comment the server wrote", "", true, false)
	comment("comment with no known run", "", false, true)
	comment("marked comment from the chat turn", r.chatTurn, true, true)

	assertSelf(t, selfByKey(e.events(r.sub.ID)), want, names)
}

// TestReconcileRecognizesEvolutionServerWrites: reconcile, which has no
// event, recognizes the Evolution agent's server writes from what the
// Evolution package records: the issue of a run or review, a proposal, and a
// comment with no run on a proposal. Anything else with no known run stays
// the Super Agent's own.
func TestReconcileRecognizesEvolutionServerWrites(t *testing.T) {
	e := newEnv(t)
	r := e.superAgentRuns()

	byAgent := func(name string, over ...testutil.Cols) string {
		cols := testutil.Cols{"parent_issue_id": r.child, "creator_type": "agent", "creator_id": e.agentA}
		for _, o := range over {
			for k, v := range o {
				cols[k] = v
			}
		}
		return e.issue(name, cols)
	}
	runIssue := byAgent("analysis issue")
	e.fx.Insert(t, "evolution_run", testutil.Cols{
		"workspace_id": e.ws, "agent_id": e.agentA, "issue_id": runIssue, "kind": "scheduled",
		"window_until": testutil.Raw("now()"),
	})
	reviewIssue := byAgent("condense review issue")
	e.fx.Insert(t, "evolution_review", testutil.Cols{
		"workspace_id": e.ws, "agent_id": e.agentA, "issue_id": reviewIssue, "kind": "condense",
		"period": testutil.Raw("date_trunc('month', now())::date"),
	})
	// A run of another agent does not make agent A's issue a server write.
	otherRunIssue := byAgent("issue linked to another agent's run")
	e.fx.Insert(t, "evolution_run", testutil.Cols{
		"workspace_id": e.ws, "agent_id": e.agentB, "issue_id": otherRunIssue, "kind": "scheduled",
		"window_until": testutil.Raw("now()"),
	})
	proposal := byAgent("proposal issue", testutil.Cols{"metadata": proposalMeta})
	plain := byAgent("issue with no known run")
	fromChat := byAgent("proposal-like issue quick-created from the chat turn",
		testutil.Cols{"metadata": proposalMeta, "origin_type": "quick_create", "origin_id": r.chatTurn})

	evidence := e.agentComment(proposal, "")
	chatOnProposal := e.agentComment(proposal, r.chatTurn)
	plainComment := e.agentComment(r.child, "")
	e.reconcile()

	assertSelf(t, selfByKey(e.events(r.sub.ID)), map[string]bool{
		runIssue: false, reviewIssue: false, proposal: false, otherRunIssue: true, plain: true, fromChat: true,
		evidence: false, chatOnProposal: true, plainComment: true,
	}, map[string]string{
		runIssue: "analysis issue", reviewIssue: "condense review issue", proposal: "proposal issue",
		otherRunIssue: "issue linked to another agent's run", plain: "issue with no known run",
		fromChat: "proposal-like issue from the chat turn", evidence: "comment with no run on a proposal",
		chatOnProposal: "comment from the chat turn on a proposal", plainComment: "comment with no run elsewhere",
	})
}

// TestEvolutionProposalStartsTheEvolutionSuperAgentsBatch is the reported
// case: the Evolution agent is the Super Agent of the project its output
// lands in. A proposal the server files in its name starts a batch in its
// chat; what it does in that chat never does.
func TestEvolutionProposalStartsTheEvolutionSuperAgentsBatch(t *testing.T) {
	e, d, calls := newDispatchEnv(t)
	d.Chat = true
	project := e.fx.Project(t, "evolution output")
	sub := e.subscribe(e.agentA, ScopeProject, project)
	board := e.issue("board", testutil.Cols{"project_id": project})
	chat := e.fx.ChatSession(t, e.agentA)
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{
		AnchorIssueID: &board, AnchorChatSessionID: &chat, Change: testChange,
	}); err != nil {
		t.Fatalf("bind the project scope: %v", err)
	}
	chatTurn := e.fx.Task(t, e.agentA, testutil.Cols{
		"chat_session_id": chat, "runtime_id": e.runtimeOf(e.agentA),
		"status": "running", "started_at": testutil.Raw("now()"),
	})

	// The Super Agent files an issue from its chat: its own, no batch.
	own := e.issue("issue from the Super Agent's chat", testutil.Cols{
		"project_id": project, "creator_type": "agent", "creator_id": e.agentA,
		"origin_type": "agent_create", "origin_id": chatTurn,
	})
	e.publishAgentIssueCreated(own, chatTurn, false)
	e.age(sub.ID)
	e.tick(d)
	if len(calls.calls) != 0 {
		t.Fatalf("the Super Agent's own issue started %d run(s)", len(calls.calls))
	}

	proposal := e.issue("proposal", testutil.Cols{
		"project_id": project, "creator_type": "agent", "creator_id": e.agentA, "metadata": proposalMeta,
	})
	e.publishAgentIssueCreated(proposal, "", true)
	e.age(sub.ID)
	e.tick(d)
	if len(calls.calls) != 1 {
		t.Fatalf("runs = %d, want 1 for the proposal the server filed", len(calls.calls))
	}
	if c := calls.calls[0]; c.chat != chat || !strings.Contains(c.note, "1 new event(s)") {
		t.Fatalf("batch is not the proposal alone in the Super Agent's chat: chat %s\n%s", c.chat, c.note)
	}
}
