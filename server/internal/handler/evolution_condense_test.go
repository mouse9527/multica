// Added by the mouse9527/multica fork: Evolution P6 (M-140).

package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// condenseFixture is an open condense review listing one target agent,
// with the review task running.
type condenseFixture struct {
	evo, target, reviewID, reviewIssueID, taskID string
	instructions                                 string
	// private and memberOnly have long instructions but are not visible to
	// every member, so the review never lists them.
	private, memberOnly string
}

// longInstructions has n distinct lines, comfortably over the lowest
// condense_min_runes.
func longInstructions(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("Rule %03d: keep the release notes for component %03d short and link the merged pull request.", i, i)
	}
	return strings.Join(lines, "\n")
}

func cleanupProposalIssues(t *testing.T) {
	t.Cleanup(func() {
		ctx := context.Background()
		proposals := `SELECT id FROM issue WHERE workspace_id = $1 AND metadata ? 'evolution_fingerprint'`
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id IN (`+proposals+`)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM issue_to_label WHERE issue_id IN (`+proposals+`)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id IN (`+proposals+`)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM issue_label WHERE workspace_id = $1 AND resource_type = 'issue' AND lower(name) = $2`,
			testWorkspaceID, evolution.ProposalLabel)
	})
}

// visibleAgent is an agent every member can see: public_to with the whole
// workspace as its target.
func visibleAgent(t *testing.T, name, instructions string) string {
	t.Helper()
	id := dbfx.Agent(t, name, handlerTestRuntimeID(t), testutil.Cols{
		"instructions": instructions, "visibility": "workspace", "permission_mode": "public_to",
	})
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id": id, "target_type": "workspace", "target_id": testWorkspaceID,
	})
	return id
}

func newCondenseFixture(t *testing.T) condenseFixture {
	t.Helper()
	var f condenseFixture
	f.evo = enableEvolutionAgent(t)
	cleanupProposalIssues(t)
	f.instructions = longInstructions(20)
	f.target = visibleAgent(t, "evolution condense target", f.instructions)
	// Long instructions too, but not every member can see these agents.
	f.private = dbfx.Agent(t, "evolution condense private", handlerTestRuntimeID(t), testutil.Cols{"instructions": f.instructions})
	f.memberOnly = dbfx.Agent(t, "evolution condense member only", handlerTestRuntimeID(t), testutil.Cols{
		"instructions": f.instructions, "visibility": "workspace", "permission_mode": "public_to",
	})
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id": f.memberOnly, "target_type": "member", "target_id": testUserID,
	})

	cfg := evolution.DefaultConfig()
	cfg.Enabled = true
	// Midnight on the 1st: this month's review is due whenever the test runs.
	cfg.Schedule.Time = "00:00"
	cfg.CondenseDayOfMonth = 1
	cfg.CondenseMinRunes = evolution.MinCondenseMinRunes
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	tickEvolution(t, nil)

	dbfx.QueryRow(t, `SELECT id::text, issue_id::text FROM evolution_review WHERE workspace_id = $1 AND status = 'open'`,
		testWorkspaceID).Scan(&f.reviewID, &f.reviewIssueID)
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 ORDER BY created_at DESC LIMIT 1`,
		f.reviewIssueID, f.evo).Scan(&f.taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', dispatched_at = now(), started_at = now() WHERE id = $1`, f.taskID)
	return f
}

// condensed is the fixture's instructions said in fewer words.
const condensed = "Keep each component's release notes short and link the merged pull request."

func (f condenseFixture) proposal() map[string]any {
	return map[string]any{
		"target":          map[string]any{"type": "agent", "id": f.target},
		"field":           "instructions",
		"topic":           "whatever the agent writes",
		"title":           "Merge twenty release-note rules into one",
		"problem":         "Fact: the twenty rules say the same thing for different components.",
		"change":          map[string]any{"before": f.instructions, "after": condensed},
		"evidence":        []map[string]any{},
		"expected_effect": "Same behavior from a fraction of the instructions.",
		"risk":            "low",
		"risk_reason":     "No rule is dropped.",
		"rollback":        "Roll back the apply.",
	}
}

// TestEvolutionCondenseReviewGoesThroughApproval: the monthly review lists
// an agent with long instructions; its run files a shorter rewrite without
// evidence (the server attaches the measurement); the proposal changes
// nothing until an owner applies it through the same apply and version
// chain as any other proposal.
func TestEvolutionCondenseReviewGoesThroughApproval(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newCondenseFixture(t)

	var candidates string
	dbfx.QueryRow(t, `SELECT candidates::text FROM evolution_review WHERE id = $1`, f.reviewID).Scan(&candidates)
	if !strings.Contains(candidates, f.target) {
		t.Fatalf("the review does not list the target: %s", candidates)
	}
	var reviewBody string
	dbfx.QueryRow(t, `SELECT description FROM issue WHERE id = $1`, f.reviewIssueID).Scan(&reviewBody)
	if !strings.Contains(reviewBody, f.target) {
		t.Errorf("the review issue does not list the target:\n%s", reviewBody)
	}
	// A review reads no signals.
	signalsPage(t, f.evo, f.taskID, evolution.SourceComment, "", 0).Want(http.StatusConflict)

	var res evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&res)
	var body, runID, fp string
	dbfx.QueryRow(t, `SELECT description, metadata->>'evolution_run_id', metadata->>'evolution_fingerprint' FROM issue WHERE id = $1`,
		res.IssueID).Scan(&body, &runID, &fp)
	if runID != f.reviewID {
		t.Errorf("evolution_run_id = %q, want the review %q", runID, f.reviewID)
	}
	if fp != evolution.Fingerprint(evolution.TargetAgent, f.target, "instructions", evolution.CondenseTopic) {
		t.Errorf("the fingerprint must use the server's topic")
	}
	if !strings.Contains(body, fmt.Sprintf("%d characters", len(f.instructions))) || !strings.Contains(body, fmt.Sprintf("After this change: %d characters", len(condensed))) {
		t.Errorf("the body lacks the measurement:\n%s", body)
	}
	if got := agentInstructions(t, f.target); got != f.instructions {
		t.Fatal("filing a condense proposal changed the agent")
	}

	// Filed again, it is the same proposal.
	var again evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusOK).JSON(&again)
	if again.Outcome != evolution.OutcomeDuplicate || again.IssueID != res.IssueID {
		t.Fatalf("same condense proposal: %+v", again)
	}

	changeProposal(t, testUserID, res.IssueID, "apply").Want(http.StatusOK)
	if got := agentInstructions(t, f.target); got != condensed {
		t.Fatalf("instructions after apply: %q", got)
	}
	if n := countVersions(t, res.IssueID); n != 1 {
		t.Fatalf("%d version rows after apply, want 1", n)
	}
	changeProposal(t, testUserID, res.IssueID, "rollback").Want(http.StatusOK)
	if got := agentInstructions(t, f.target); got != f.instructions {
		t.Fatalf("instructions after rollback: %q", got)
	}
}

// TestEvolutionCondenseReviewRejections: a review files only shorter
// instructions of the agents it lists, and cites no evidence.
func TestEvolutionCondenseReviewRejections(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newCondenseFixture(t)

	longer := f.proposal()
	longer["change"] = map[string]any{"before": "Rule 000:", "after": "Rule 000, which matters most of all the rules here:"}
	wantRejected(t, propose(t, f.evo, f.taskID, longer), http.StatusUnprocessableEntity, evolution.RejectNotShorter)

	withEvidence := f.proposal()
	withEvidence["evidence"] = []map[string]any{{"type": "issue", "id": dbfx.Issue(t, "evolution condense evidence"), "note": "n"}}
	wantRejected(t, propose(t, f.evo, f.taskID, withEvidence), http.StatusUnprocessableEntity, evolution.RejectInvalid)

	unlisted := f.proposal()
	other := dbfx.Agent(t, "evolution condense unlisted", handlerTestRuntimeID(t), testutil.Cols{"instructions": "Short and sweet."})
	unlisted["target"] = map[string]any{"type": "agent", "id": other}
	unlisted["change"] = map[string]any{"before": "Short and sweet.", "after": "Short."}
	wantRejected(t, propose(t, f.evo, f.taskID, unlisted), http.StatusUnprocessableEntity, evolution.RejectTarget)

	description := f.proposal()
	description["field"] = "description"
	wantRejected(t, propose(t, f.evo, f.taskID, description), http.StatusUnprocessableEntity, evolution.RejectField)

	if n := countProposals(t); n != 0 {
		t.Fatalf("rejected proposals opened %d issues", n)
	}
}

// TestEvolutionCondenseReviewListsOnlyWorkspaceVisibleAgents: a private
// agent and one shared with only some members never appear in the review
// issue, which every member reads, and cannot be proposed for; nor can a
// listed agent made private after the review opened.
func TestEvolutionCondenseReviewListsOnlyWorkspaceVisibleAgents(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newCondenseFixture(t)

	var candidates, body string
	dbfx.QueryRow(t, `SELECT candidates::text FROM evolution_review WHERE id = $1`, f.reviewID).Scan(&candidates)
	dbfx.QueryRow(t, `SELECT description FROM issue WHERE id = $1`, f.reviewIssueID).Scan(&body)
	for _, hidden := range []string{f.private, f.memberOnly} {
		if strings.Contains(candidates, hidden) || strings.Contains(body, hidden) {
			t.Errorf("agent %s is not visible to every member but is on the review", hidden)
		}
	}
	for _, name := range []string{"evolution condense private", "evolution condense member only"} {
		if strings.Contains(body, name) {
			t.Errorf("the review issue names %q", name)
		}
	}

	for _, hidden := range []string{f.private, f.memberOnly} {
		p := f.proposal()
		p["target"] = map[string]any{"type": "agent", "id": hidden}
		wantRejected(t, propose(t, f.evo, f.taskID, p), http.StatusUnprocessableEntity, evolution.RejectTarget)
	}

	// Made private after the review listed it.
	dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, f.target)
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, f.target)
	wantRejected(t, propose(t, f.evo, f.taskID, f.proposal()), http.StatusUnprocessableEntity, evolution.RejectTarget)
	if n := countProposals(t); n != 0 {
		t.Fatalf("%d proposal issues for agents not every member can see", n)
	}
}

// TestEvolutionCondenseReviewOncePerMonth: the review settles when its task
// ends, and the same month does not get a second one.
func TestEvolutionCondenseReviewOncePerMonth(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newCondenseFixture(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, f.taskID)
	tickEvolution(t, nil)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM evolution_review WHERE id = $1`, f.reviewID).Scan(&status)
	if status != evolution.ReviewSucceeded {
		t.Fatalf("review status %q after its task completed", status)
	}
	tickEvolution(t, nil)
	if n := dbfx.Count(t, `SELECT count(*) FROM evolution_review WHERE workspace_id = $1`, testWorkspaceID); n != 1 {
		t.Fatalf("%d reviews this month, want 1", n)
	}
	// A settled review files nothing, even from a new task on its issue.
	retry := dbfx.Task(t, f.evo, testutil.Cols{
		"status": "running", "runtime_id": handlerTestRuntimeID(t), "issue_id": f.reviewIssueID,
	})
	propose(t, f.evo, retry, f.proposal()).Want(http.StatusConflict)
}

// TestEvolutionCondenseReviewDisabled: with condense_disabled no review
// opens, and with nothing over a threshold the month is skipped without an
// issue.
func TestEvolutionCondenseReviewDisabled(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableEvolutionAgent(t)
	cfg := evolution.DefaultConfig()
	cfg.Enabled = true
	cfg.Schedule.Time = "00:00"
	cfg.CondenseDisabled = true
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	tickEvolution(t, nil)
	if n := dbfx.Count(t, `SELECT count(*) FROM evolution_review WHERE workspace_id = $1`, testWorkspaceID); n != 0 {
		t.Fatalf("%d reviews with the review disabled", n)
	}

	cfg.CondenseDisabled = false
	cfg.CondenseMinRunes = evolution.MaxCondenseMinRunes
	cfg.CondenseRepeatPercent = evolution.MaxCondenseRepeatPercent
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	tickEvolution(t, nil)
	var status string
	var hasIssue bool
	dbfx.QueryRow(t, `SELECT status, issue_id IS NOT NULL FROM evolution_review WHERE workspace_id = $1`, testWorkspaceID).
		Scan(&status, &hasIssue)
	if status != evolution.ReviewSkipped || hasIssue {
		t.Fatalf("review %q (issue: %v), want skipped without an issue", status, hasIssue)
	}
}

// TestEvolutionTeamProposalIsTextOnly: a proposal to create, merge, or
// re-route agents files an issue and nothing else: no stored change, no
// apply, no agent or squad written.
func TestEvolutionTeamProposalIsTextOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	countTeam := func() int {
		return dbfx.Count(t, `SELECT (SELECT count(*) FROM agent WHERE workspace_id = $1) + (SELECT count(*) FROM squad WHERE workspace_id = $1)`,
			testWorkspaceID)
	}
	before := countTeam()

	team := f.proposal()
	team["target"] = map[string]any{"type": "team", "id": testWorkspaceID}
	team["field"] = "merge_agents"
	team["topic"] = "two agents for one job"
	team["title"] = "Merge the two release agents"
	team["change"] = map[string]any{"before": "", "after": "Merge the release agent and the changelog agent into one agent."}
	var res evolution.FileResult
	propose(t, f.evo, f.taskID, team).Want(http.StatusCreated).JSON(&res)

	var body string
	dbfx.QueryRow(t, `SELECT description FROM issue WHERE id = $1`, res.IssueID).Scan(&body)
	if !strings.Contains(body, "Carried out by hand") {
		t.Errorf("a team proposal must say it is carried out by hand:\n%s", body)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM evolution_proposal WHERE issue_id = $1`, res.IssueID); n != 0 {
		t.Errorf("a team proposal stored %d changes to apply", n)
	}
	evolutionCall(t, testUserID, newRequest("POST", proposalRoute(res.IssueID, "apply"), nil)).Want(http.StatusNotFound)
	if got := countTeam(); got != before {
		t.Errorf("agents and squads went from %d to %d", before, got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, res.IssueID); n != 0 {
		t.Errorf("a team proposal queued %d tasks", n)
	}

	elsewhere := f.proposal()
	elsewhere["target"] = map[string]any{"type": "team", "id": f.target}
	elsewhere["field"] = "new_agent"
	elsewhere["change"] = map[string]any{"before": "", "after": "Add a triage agent."}
	wantRejected(t, propose(t, f.evo, f.taskID, elsewhere), http.StatusUnprocessableEntity, evolution.RejectTarget)
}
