// Added by the mouse9527/multica fork: Evolution P3 (M-137).

package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const proposalTargetInstructions = "Always reply in English.\nClose the issue when the work is merged."

// proposalFixture is an open analysis run with a target agent and evidence
// from two different issues and one chat.
type proposalFixture struct {
	evo, taskID, runIssueID string
	target                  string
	commentA, commentB      string
	issueA                  string
	chatMessage             string
}

const proposalChatText = "please stop closing my issues before the pull request is actually merged into main"

// proposalLongComment is longer than a quote may be.
const proposalLongComment = "The release checklist says to wait for CI, then tag, then publish the notes; " +
	"but the agent tagged before CI finished three times this week, and each time the tag had to be deleted and redone by hand."

func newProposalFixture(t *testing.T, cfg evolution.Config) proposalFixture {
	t.Helper()
	var f proposalFixture
	f.evo = enableEvolutionAgent(t)
	t.Cleanup(func() {
		ctx := context.Background()
		proposals := `SELECT id FROM issue WHERE workspace_id = $1 AND metadata ? 'evolution_fingerprint'`
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id IN (`+proposals+`)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM issue_to_label WHERE issue_id IN (`+proposals+`)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id IN (`+proposals+`)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM issue_label WHERE workspace_id = $1 AND resource_type = 'issue' AND lower(name) = $2`,
			testWorkspaceID, evolution.ProposalLabel)
	})
	cfg.Enabled = true
	cfg.Sources.Chat = true
	putEvolutionConfig(t, cfg, true).Want(http.StatusOK)
	tickEvolution(t, nil)
	_, f.taskID = openEvolutionRunTask(t, f.evo)
	dbfx.QueryRow(t, `SELECT issue_id::text FROM evolution_run WHERE workspace_id = $1 AND status = 'open'`,
		testWorkspaceID).Scan(&f.runIssueID)

	runtimeID := handlerTestRuntimeID(t)
	f.target = dbfx.Agent(t, "evolution proposal target", runtimeID, testutil.Cols{"instructions": proposalTargetInstructions})
	f.issueA = dbfx.Issue(t, "evolution proposal evidence A")
	issueB := dbfx.Issue(t, "evolution proposal evidence B")
	f.commentA = dbfx.Comment(t, f.issueA, "The agent closed this issue before the PR was merged, so review never happened.")
	f.commentB = dbfx.Comment(t, issueB, "Closed too early again; the PR is still open.")

	publicAgent := dbfx.Agent(t, "evolution proposal chat agent", runtimeID, testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to",
	})
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id": publicAgent, "target_type": "workspace", "target_id": testWorkspaceID,
	})
	f.chatMessage = dbfx.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": dbfx.ChatSession(t, publicAgent), "role": "user", "content": proposalChatText,
	})
	return f
}

func (f proposalFixture) proposal() map[string]any {
	return map[string]any{
		"target":  map[string]any{"type": "agent", "id": f.target},
		"field":   "instructions",
		"topic":   "Closes issues before merge",
		"title":   "Close issues only after the PR is merged",
		"problem": "Fact: issues were closed while their PRs were still open (two issues). Inference: the instruction says merged but does not say to check.",
		"change":  map[string]any{"before": "Close the issue when the work is merged.", "after": "Close the issue only after checking that its PR is merged."},
		"evidence": []map[string]any{
			{"type": "comment", "id": f.commentA, "note": "closed before merge", "quote": "closed this issue before the PR was merged"},
			{"type": "comment", "id": f.commentB, "note": "the same again"},
		},
		"expected_effect": "Fewer issues reopened after closing, over two weeks.",
		"risk":            "low",
		"risk_reason":     "One sentence of instructions.",
		"rollback":        "Restore the previous sentence.",
	}
}

func propose(t *testing.T, agentID, taskID string, body any) *testutil.Response {
	t.Helper()
	req := newRequest("POST", "/api/evolution/proposals", body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	return evolutionCall(t, testUserID, req)
}

func wantRejected(t *testing.T, resp *testutil.Response, status int, code string) {
	t.Helper()
	var got evolution.RejectedError
	resp.Want(status).JSON(&got)
	if got.Code != code || got.Message == "" {
		t.Fatalf("want rejection %q with a reason, got %+v", code, got)
	}
}

func countProposals(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND metadata ? 'evolution_fingerprint'`, testWorkspaceID)
}

// TestEvolutionProposalCreatesIssueAndAppliesNothing: a valid proposal
// becomes one new, unassigned issue with the label and the flat metadata
// keys; the target agent is not touched and no task is queued.
func TestEvolutionProposalCreatesIssueAndAppliesNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())

	var res evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&res)
	if res.Outcome != evolution.OutcomeCreated || res.IssueID == "" || res.ProposalsLeft != evolution.DefaultProposalsPerRun-1 {
		t.Fatalf("unexpected result %+v", res)
	}

	var title, description, metaTarget, metaField, metaRun, metaFP, metaHash string
	var assigned bool
	dbfx.QueryRow(t, `SELECT title, description, assignee_id IS NOT NULL,
			metadata->>'evolution_target_id', metadata->>'evolution_field', metadata->>'evolution_run_id',
			metadata->>'evolution_fingerprint', metadata->>'evolution_base_hash'
		FROM issue WHERE id = $1`, res.IssueID).
		Scan(&title, &description, &assigned, &metaTarget, &metaField, &metaRun, &metaFP, &metaHash)
	if assigned {
		t.Error("a proposal must not be assigned")
	}
	if metaTarget != f.target || metaField != "instructions" || metaFP != res.Fingerprint || metaHash == "" || metaRun == "" {
		t.Errorf("metadata: target %q field %q run %q fingerprint %q hash %q", metaTarget, metaField, metaRun, metaFP, metaHash)
	}
	if !strings.Contains(title, "Close issues only after the PR is merged") {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{"- Close the issue when the work is merged.", "+ Close the issue only after checking", f.commentA, f.commentB, f.runIssueID} {
		if !strings.Contains(description, want) {
			t.Errorf("description lacks %q:\n%s", want, description)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_to_label x JOIN issue_label l ON l.id = x.label_id
		WHERE x.issue_id = $1 AND l.name = $2`, res.IssueID, evolution.ProposalLabel); n != 1 {
		t.Errorf("proposal has %d evolution labels", n)
	}
	var instructions string
	dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, f.target).Scan(&instructions)
	if instructions != proposalTargetInstructions {
		t.Errorf("the target was changed: %q", instructions)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, res.IssueID); n != 0 {
		t.Errorf("a proposal queued %d tasks", n)
	}
}

// TestEvolutionProposalDuplicateMergesIntoOpenIssue: filing the same
// proposal again opens no issue; new evidence is added to the open one as a
// comment; with nothing new, nothing is written.
func TestEvolutionProposalDuplicateMergesIntoOpenIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	var first evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&first)

	var again evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusOK).JSON(&again)
	if again.Outcome != evolution.OutcomeDuplicate || again.IssueID != first.IssueID {
		t.Fatalf("same proposal: %+v", again)
	}

	more := f.proposal()
	more["topic"] = "closes issues  before MERGE!" // same topic, worded differently
	issueC := dbfx.Issue(t, "evolution proposal evidence C")
	more["evidence"] = append(more["evidence"].([]map[string]any), map[string]any{"type": "issue", "id": issueC, "note": "a third case"})
	var merged evolution.FileResult
	propose(t, f.evo, f.taskID, more).Want(http.StatusOK).JSON(&merged)
	if merged.Outcome != evolution.OutcomeMerged || merged.IssueID != first.IssueID || merged.NewEvidence != 1 {
		t.Fatalf("merge: %+v", merged)
	}
	if n := countProposals(t); n != 1 {
		t.Fatalf("%d proposal issues, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_id = $2 AND strpos(content, $3::text) > 0`,
		first.IssueID, f.evo, issueC); n != 1 {
		t.Errorf("new evidence comments: %d", n)
	}
}

// TestEvolutionProposalRecentlyRejected: a proposal an owner cancelled is
// not filed again within the cooldown.
func TestEvolutionProposalRecentlyRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	var first evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&first)
	dbfx.Exec(t, `UPDATE issue SET status = 'cancelled' WHERE id = $1`, first.IssueID)

	wantRejected(t, propose(t, f.evo, f.taskID, f.proposal()), http.StatusConflict, evolution.RejectRecentlyRejected)
	if n := countProposals(t); n != 1 {
		t.Fatalf("%d proposal issues, want 1", n)
	}
}

// TestEvolutionProposalRunLimit: one run opens at most max_proposals_per_run
// new proposals.
func TestEvolutionProposalRunLimit(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cfg := evolution.DefaultConfig()
	cfg.MaxProposalsPerRun = 1
	f := newProposalFixture(t, cfg)
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated)

	second := f.proposal()
	second["topic"] = "a different problem"
	wantRejected(t, propose(t, f.evo, f.taskID, second), http.StatusConflict, evolution.RejectRunLimit)
	if n := countProposals(t); n != 1 {
		t.Fatalf("%d proposal issues, want 1", n)
	}
}

// TestEvolutionProposalRejections: every broken rule is refused with its
// code and opens nothing.
func TestEvolutionProposalRejections(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	private := dbfx.Project(t, "evolution proposal private", testutil.Cols{"visibility": "private"})
	privateComment := dbfx.Comment(t, dbfx.Issue(t, "evolution proposal private issue", testutil.Cols{"project_id": private}), "private finding")
	dbfx.Member(t, testWorkspaceID, dbfx.User(t, "Zelda Quartz", "zelda-quartz-evolution@example.test"), "member")
	longComment := dbfx.Comment(t, dbfx.Issue(t, "evolution proposal evidence long"), proposalLongComment)
	longRunes := []rune(proposalLongComment)

	evidence := func(items ...map[string]any) []map[string]any { return items }
	cases := []struct {
		name   string
		edit   func(p map[string]any)
		status int
		code   string
	}{
		{"self target", func(p map[string]any) { p["target"] = map[string]any{"type": "agent", "id": f.evo} }, http.StatusUnprocessableEntity, evolution.RejectTarget},
		{"unknown target type", func(p map[string]any) { p["target"] = map[string]any{"type": "runtime", "id": f.target} }, http.StatusUnprocessableEntity, evolution.RejectTarget},
		{"forbidden field", func(p map[string]any) { p["field"] = "mcp_config" }, http.StatusUnprocessableEntity, evolution.RejectField},
		{"field off the whitelist", func(p map[string]any) { p["field"] = "model" }, http.StatusUnprocessableEntity, evolution.RejectField},
		{"one source", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA}, map[string]any{"type": "issue", "id": f.issueA})
		}, http.StatusUnprocessableEntity, evolution.RejectEvidence},
		{"unreadable evidence", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA}, map[string]any{"type": "comment", "id": privateComment})
		}, http.StatusUnprocessableEntity, evolution.RejectEvidence},
		{"quoting a chat", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA}, map[string]any{"type": "chat", "id": f.chatMessage, "quote": "stop closing my issues"})
		}, http.StatusUnprocessableEntity, evolution.RejectQuote},
		{"pasting a chat", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA}, map[string]any{"type": "chat", "id": f.chatMessage, "note": "a member asked to wait for the merge"})
			p["problem"] = "A member wrote: " + proposalChatText
		}, http.StatusUnprocessableEntity, evolution.RejectQuote},
		{"copying a cited comment past 120 characters", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA}, map[string]any{"type": "comment", "id": longComment, "note": "tagged before CI"})
			p["problem"] = "As a comment put it: " + string(longRunes[5:5+evolution.MaxQuoteRunes+1])
		}, http.StatusUnprocessableEntity, evolution.RejectQuote},
		{"quote too long", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA, "quote": strings.Repeat("x", evolution.MaxQuoteRunes+1)}, map[string]any{"type": "comment", "id": f.commentB})
		}, http.StatusUnprocessableEntity, evolution.RejectQuote},
		{"made-up quote", func(p map[string]any) {
			p["evidence"] = evidence(map[string]any{"type": "comment", "id": f.commentA, "quote": "something nobody wrote"}, map[string]any{"type": "comment", "id": f.commentB})
		}, http.StatusUnprocessableEntity, evolution.RejectQuote},
		{"member name", func(p map[string]any) { p["problem"] = "Zelda Quartz keeps reopening these issues." }, http.StatusUnprocessableEntity, evolution.RejectSensitive},
		{"secret", func(p map[string]any) { p["rollback"] = "Use token mul_abcdefghijklmnop1234 to revert." }, http.StatusUnprocessableEntity, evolution.RejectSensitive},
		{"stale before", func(p map[string]any) {
			p["change"] = map[string]any{"before": "Text the agent does not have.", "after": "New text."}
		}, http.StatusUnprocessableEntity, evolution.RejectStaleBefore},
		{"unknown field in body", func(p map[string]any) { p["apply"] = true }, http.StatusBadRequest, evolution.RejectInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.proposal()
			tc.edit(p)
			wantRejected(t, propose(t, f.evo, f.taskID, p), tc.status, tc.code)
		})
	}
	if n := countProposals(t); n != 0 {
		t.Fatalf("rejected proposals opened %d issues", n)
	}
}

// TestEvolutionProposalOnlyForTheRunTask: members and other agents cannot
// file proposals; the Evolution agent outside an open run cannot either.
func TestEvolutionProposalOnlyForTheRunTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())

	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/proposals", f.proposal())).Want(http.StatusForbidden)

	runtimeID := handlerTestRuntimeID(t)
	other := dbfx.Agent(t, "evolution proposal other agent", runtimeID)
	otherTask := dbfx.Task(t, other, testutil.Cols{
		"status": "running", "runtime_id": runtimeID, "issue_id": dbfx.Issue(t, "evolution proposal other"),
	})
	propose(t, other, otherTask, f.proposal()).Want(http.StatusForbidden)

	looseTask := dbfx.Task(t, f.evo, testutil.Cols{
		"status": "running", "runtime_id": runtimeID, "issue_id": dbfx.Issue(t, "evolution proposal loose"),
	})
	propose(t, f.evo, looseTask, f.proposal()).Want(http.StatusConflict)
	if n := countProposals(t); n != 0 {
		t.Fatalf("%d proposal issues, want 0", n)
	}
}

// TestEvolutionProposalCitesOnlySourcesBeingRead: with chat reading off
// (members not told), a chat cannot be cited.
func TestEvolutionProposalCitesOnlySourcesBeingRead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cfg := evolution.DefaultConfig()
	f := newProposalFixture(t, cfg)
	cfg.Enabled = true
	cfg.Sources.Chat = false
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)

	p := f.proposal()
	p["evidence"] = []map[string]any{
		{"type": "comment", "id": f.commentA},
		{"type": "chat", "id": f.chatMessage, "note": "a member asked to wait for the merge"},
	}
	wantRejected(t, propose(t, f.evo, f.taskID, p), http.StatusUnprocessableEntity, evolution.RejectEvidence)
	if n := countProposals(t); n != 0 {
		t.Fatalf("%d proposal issues, want 0", n)
	}
}

// TestEvolutionProposalChecksWhatTheRunRead: text the run was served but
// does not cite cannot be pasted in either — a chat message (any 20
// characters) or more than 120 characters of a comment, in any field.
func TestEvolutionProposalChecksWhatTheRunRead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	backdateEvolutionCursors(t)

	hourAgo := testutil.Cols{"created_at": testutil.Raw("now() - interval '1 hour'")}
	runtimeID := handlerTestRuntimeID(t)
	chatAgent := dbfx.Agent(t, "evolution proposal served chat agent", runtimeID, testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to",
	})
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id": chatAgent, "target_type": "workspace", "target_id": testWorkspaceID,
	})
	const servedChat = "kindly hold every deploy until the nightly smoke suite has finished running"
	chatID := dbfx.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": dbfx.ChatSession(t, chatAgent), "role": "user", "content": servedChat,
		"created_at": testutil.Raw("now() - interval '1 hour'"),
	})
	commentID := dbfx.Comment(t, dbfx.Issue(t, "evolution proposal served comment"), proposalLongComment, hourAgo)

	chats, _ := readAllSignals(t, f.evo, f.taskID, evolution.SourceChat)
	comments, _ := readAllSignals(t, f.evo, f.taskID, evolution.SourceComment)
	if _, ok := chats[chatID]; !ok {
		t.Fatal("the run should have been served the chat message")
	}
	if _, ok := comments[commentID]; !ok {
		t.Fatal("the run should have been served the comment")
	}

	// Neither is cited: the default proposal cites two other comments.
	p := f.proposal()
	p["problem"] = "A member asked us to " + servedChat + "."
	wantRejected(t, propose(t, f.evo, f.taskID, p), http.StatusUnprocessableEntity, evolution.RejectQuote)

	p = f.proposal()
	p["expected_effect"] = string([]rune(proposalLongComment)[:evolution.MaxQuoteRunes+10])
	wantRejected(t, propose(t, f.evo, f.taskID, p), http.StatusUnprocessableEntity, evolution.RejectQuote)

	if n := countProposals(t); n != 0 {
		t.Fatalf("%d proposal issues, want 0", n)
	}
	// What the run read does not get in the way of a proposal that only
	// summarizes it.
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated)
}
