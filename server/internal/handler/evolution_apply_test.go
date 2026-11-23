// Added by the mouse9527/multica fork: Evolution P4 (M-138).

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// proposalTargetApplied is proposalTargetInstructions with the fixture
// proposal's change.
const proposalTargetApplied = "Always reply in English.\nClose the issue only after checking that its PR is merged."

// fileProposal files body from the fixture's run and returns the proposal
// issue.
func fileProposal(t *testing.T, f proposalFixture, body map[string]any) string {
	t.Helper()
	var res evolution.FileResult
	propose(t, f.evo, f.taskID, body).Want(http.StatusCreated).JSON(&res)
	return res.IssueID
}

func proposalRoute(issueID, action string) string {
	if action == "" {
		return "/api/evolution/proposals/" + issueID
	}
	return "/api/evolution/proposals/" + issueID + "/" + action
}

// changeProposal posts apply or rollback as the given member.
func changeProposal(t *testing.T, userID, issueID, action string) *testutil.Response {
	t.Helper()
	return evolutionCall(t, userID, newRequest("POST", proposalRoute(issueID, action), nil))
}

func proposalState(t *testing.T, userID, issueID string) evolutionProposalResponse {
	t.Helper()
	var resp evolutionProposalResponse
	evolutionCall(t, userID, newRequest("GET", proposalRoute(issueID, ""), nil)).Want(http.StatusOK).JSON(&resp)
	return resp
}

func agentInstructions(t *testing.T, agentID string) string {
	t.Helper()
	var instructions string
	dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, agentID).Scan(&instructions)
	return instructions
}

func countVersions(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM evolution_config_version WHERE proposal_issue_id = $1`, issueID)
}

func proposalStateKey(t *testing.T, issueID string) string {
	t.Helper()
	var state string
	dbfx.QueryRow(t, `SELECT COALESCE(metadata->>'evolution_state', '') FROM issue WHERE id = $1`, issueID).Scan(&state)
	return state
}

// TestEvolutionApplyAndRollback: an owner applies a proposal, which writes
// the change and appends version 1; a rollback restores the field and
// appends version 2 pointing at version 1, which stays as it was. Each step
// leaves a comment by the person who acted.
func TestEvolutionApplyAndRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())

	state := proposalState(t, testUserID, issueID)
	if state.State != "pending" || !state.CanApply || state.CanRollback || state.Stale || !state.Applicable {
		t.Fatalf("before apply: %+v", state)
	}
	if state.Change.After != "Close the issue only after checking that its PR is merged." {
		t.Fatalf("change: %+v", state.Change)
	}

	var applied evolutionProposalResponse
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusOK).JSON(&applied)
	if applied.State != "applied" || applied.CanApply || !applied.CanRollback || len(applied.Versions) != 1 ||
		applied.Versions[0].Version != 1 || applied.Versions[0].Action != evolution.ActionApply {
		t.Fatalf("after apply: %+v", applied)
	}
	if got := agentInstructions(t, f.target); got != proposalTargetApplied {
		t.Fatalf("instructions after apply: %q", got)
	}
	if got := proposalStateKey(t, issueID); got != evolution.StateApplied {
		t.Errorf("evolution_state = %q", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'member' AND author_id = $2`, issueID, testUserID); n != 1 {
		t.Errorf("apply left %d member comments", n)
	}
	var beforeHash, afterHash string
	dbfx.QueryRow(t, `SELECT before_hash, after_hash FROM evolution_config_version WHERE proposal_issue_id = $1`, issueID).Scan(&beforeHash, &afterHash)
	if beforeHash == afterHash {
		t.Fatal("an apply must change the target's hash")
	}

	wantRejected(t, changeProposal(t, testUserID, issueID, "apply"), http.StatusConflict, evolution.ApplyAlreadyApplied)

	var rolledBack evolutionProposalResponse
	changeProposal(t, testUserID, issueID, "rollback").Want(http.StatusOK).JSON(&rolledBack)
	if rolledBack.State != "rolled_back" || rolledBack.CanRollback || len(rolledBack.Versions) != 2 {
		t.Fatalf("after rollback: %+v", rolledBack)
	}
	if got := agentInstructions(t, f.target); got != proposalTargetInstructions {
		t.Fatalf("instructions after rollback: %q", got)
	}
	v1, v2 := rolledBack.Versions[0], rolledBack.Versions[1]
	if v2.Version != 2 || v2.Action != evolution.ActionRollback || v2.RevertedOf == nil || *v2.RevertedOf != v1.ID {
		t.Fatalf("rollback row: %+v", v2)
	}
	// History only grows: the apply row is untouched.
	var beforeHash2, afterHash2, action string
	dbfx.QueryRow(t, `SELECT before_hash, after_hash, action FROM evolution_config_version WHERE id = $1`, v1.ID).Scan(&beforeHash2, &afterHash2, &action)
	if beforeHash2 != beforeHash || afterHash2 != afterHash || action != evolution.ActionApply {
		t.Fatal("the apply row changed after the rollback")
	}
	var rollbackAfter string
	dbfx.QueryRow(t, `SELECT after_hash FROM evolution_config_version WHERE id = $1`, v2.ID).Scan(&rollbackAfter)
	if rollbackAfter != beforeHash {
		t.Error("a rollback must restore the hash from before the apply")
	}
	if got := proposalStateKey(t, issueID); got != evolution.StateRolledBack {
		t.Errorf("evolution_state = %q", got)
	}
	wantRejected(t, changeProposal(t, testUserID, issueID, "rollback"), http.StatusConflict, evolution.ApplyNotApplied)

	// Rolled back, the proposal applies again.
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusOK)
	if got := agentInstructions(t, f.target); got != proposalTargetApplied {
		t.Fatalf("instructions after the second apply: %q", got)
	}
	if n := countVersions(t, issueID); n != 3 {
		t.Fatalf("%d version rows, want 3", n)
	}
}

// TestEvolutionApplyOnlyForOwnersAndAdmins: agent credentials (any task
// token, the Evolution agent's own included) and plain members get 403 on
// apply and rollback, and nothing is written. With the flag off the routes
// do not exist.
func TestEvolutionApplyOnlyForOwnersAndAdmins(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())

	for _, action := range []string{"apply", "rollback"} {
		machine := newRequest("POST", proposalRoute(issueID, action), nil)
		machine.Header.Set("X-Actor-Source", "task_token")
		machine.Header.Set("X-Agent-ID", f.evo)
		machine.Header.Set("X-Task-ID", f.taskID)
		evolutionCall(t, testUserID, machine).Want(http.StatusForbidden)

		pat := newRequest("POST", proposalRoute(issueID, action), nil)
		pat.Header.Set("X-Actor-Source", "cloud_pat")
		evolutionCall(t, testUserID, pat).Want(http.StatusForbidden)
	}

	member := addSecondWorkspaceMember(t, "evolution-apply-member@multica.test")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE user_id = $1`, member)
	})
	changeProposal(t, member, issueID, "apply").Want(http.StatusForbidden)
	changeProposal(t, member, issueID, "rollback").Want(http.StatusForbidden)
	// A member may read the state but is offered nothing.
	if state := proposalState(t, member, issueID); state.CanApply || state.CanRollback {
		t.Fatalf("member state: %+v", state)
	}

	if got := agentInstructions(t, f.target); got != proposalTargetInstructions {
		t.Fatalf("a refused apply changed the target: %q", got)
	}
	if n := countVersions(t, issueID); n != 0 {
		t.Fatalf("refused calls wrote %d versions", n)
	}

	withEvolutionFlag(t, false)
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusNotFound)
}

// TestEvolutionApplyStaleIsConflict: when someone changed the target after
// the proposal was written, apply answers 409, writes nothing, and marks
// the proposal stale once.
func TestEvolutionApplyStaleIsConflict(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())

	const edited = proposalTargetInstructions + "\nMention the PR in the closing comment."
	dbfx.Exec(t, `UPDATE agent SET instructions = $2 WHERE id = $1`, f.target, edited)

	if state := proposalState(t, testUserID, issueID); !state.Stale || state.CanApply {
		t.Fatalf("state after an edit: %+v", state)
	}
	wantRejected(t, changeProposal(t, testUserID, issueID, "apply"), http.StatusConflict, evolution.ApplyStale)
	wantRejected(t, changeProposal(t, testUserID, issueID, "apply"), http.StatusConflict, evolution.ApplyStale)
	if got := agentInstructions(t, f.target); got != edited {
		t.Fatalf("a stale apply overwrote the edit: %q", got)
	}
	if n := countVersions(t, issueID); n != 0 {
		t.Fatalf("a stale apply wrote %d versions", n)
	}
	if got := proposalStateKey(t, issueID); got != evolution.StateStale {
		t.Errorf("evolution_state = %q", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'member'`, issueID); n != 1 {
		t.Errorf("%d stale notes, want 1", n)
	}
}

// TestEvolutionRollbackKeepsLaterEdits: a change made after the apply is
// never overwritten; the rollback is refused instead.
func TestEvolutionRollbackKeepsLaterEdits(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusOK)

	const edited = proposalTargetApplied + "\nAsk before closing."
	dbfx.Exec(t, `UPDATE agent SET instructions = $2 WHERE id = $1`, f.target, edited)

	if state := proposalState(t, testUserID, issueID); !state.ChangedSinceApply || state.CanRollback {
		t.Fatalf("state after a later edit: %+v", state)
	}
	wantRejected(t, changeProposal(t, testUserID, issueID, "rollback"), http.StatusConflict, evolution.ApplyChangedSinceApply)
	if got := agentInstructions(t, f.target); got != edited {
		t.Fatalf("the rollback overwrote a later edit: %q", got)
	}
	if n := countVersions(t, issueID); n != 1 {
		t.Fatalf("%d version rows, want 1", n)
	}
}

// TestEvolutionApplyRefusesFieldsOffTheWhitelist: a stored change to a field
// apply does not write, or to a squad, is refused and writes nothing, however
// it got there.
func TestEvolutionApplyRefusesFieldsOffTheWhitelist(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	var runID string
	dbfx.QueryRow(t, `SELECT id::text FROM evolution_run WHERE workspace_id = $1 AND status = 'open'`, testWorkspaceID).Scan(&runID)

	cases := []struct {
		targetType, field, code string
	}{
		{"agent", "custom_env", evolution.RejectField},
		{"agent", "custom_args", evolution.RejectField},
		{"agent", "mcp_config", evolution.RejectField},
		{"agent", "runtime_id", evolution.RejectField},
		{"agent", "permission_mode", evolution.RejectField},
		{"agent", "model", evolution.RejectField},
		{"squad", "instructions", evolution.RejectTarget},
	}
	for _, tc := range cases {
		t.Run(tc.targetType+" "+tc.field, func(t *testing.T) {
			issueID := dbfx.Issue(t, "evolution apply off the whitelist "+tc.field)
			dbfx.Exec(t, `
				INSERT INTO evolution_proposal (issue_id, workspace_id, run_id, target_type, target_id, field, base_hash,
				                                change_before, change_after, base_value, proposed_value)
				VALUES ($1, $2, $3, $4, $5, $6, 'x', '', '{"X":"1"}', '', '{"X":"1"}')`,
				issueID, testWorkspaceID, runID, tc.targetType, f.target, tc.field)
			wantRejected(t, changeProposal(t, testUserID, issueID, "apply"), http.StatusUnprocessableEntity, tc.code)
			if n := countVersions(t, issueID); n != 0 {
				t.Fatalf("wrote %d versions", n)
			}
		})
	}
	var env, mcp string
	dbfx.QueryRow(t, `SELECT COALESCE(custom_env::text, ''), COALESCE(mcp_config::text, '') FROM agent WHERE id = $1`, f.target).Scan(&env, &mcp)
	if env == `{"X":"1"}` || mcp == `{"X":"1"}` {
		t.Fatal("a field off the whitelist was written")
	}
	// An issue that is not a proposal is not found.
	changeProposal(t, testUserID, dbfx.Issue(t, "evolution apply plain issue"), "apply").Want(http.StatusNotFound)
}

// TestEvolutionApplyWorkflow: a workflow's description applies and rolls
// back; a steps change the workflow checks refuse is reported and writes
// nothing.
func TestEvolutionApplyWorkflow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	workflow := createTestWorkflow(t, "evolution apply "+workflowTestSuffix())

	body := f.proposal()
	body["target"] = map[string]any{"type": "workflow", "id": workflow.ID}
	body["field"] = "description"
	body["topic"] = "Workflow lacks a description"
	body["change"] = map[string]any{"before": "", "after": "Hand off only reviewed work."}
	issueID := fileProposal(t, f, body)
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusOK)
	var description string
	dbfx.QueryRow(t, `SELECT description FROM issue_workflow WHERE id = $1`, workflow.ID).Scan(&description)
	if description != "Hand off only reviewed work." {
		t.Fatalf("description after apply: %q", description)
	}
	changeProposal(t, testUserID, issueID, "rollback").Want(http.StatusOK)
	dbfx.QueryRow(t, `SELECT description FROM issue_workflow WHERE id = $1`, workflow.ID).Scan(&description)
	if description != "" {
		t.Fatalf("description after rollback: %q", description)
	}

	var stepsBefore string
	dbfx.QueryRow(t, `SELECT steps::text FROM issue_workflow WHERE id = $1`, workflow.ID).Scan(&stepsBefore)
	steps := f.proposal()
	steps["target"] = map[string]any{"type": "workflow", "id": workflow.ID}
	steps["field"] = "steps"
	steps["topic"] = "Blocked step is unused"
	steps["change"] = map[string]any{"before": `"status_key": "blocked"`, "after": `"status_key": "no_such_status"`}
	stepsIssue := fileProposal(t, f, steps)
	wantRejected(t, changeProposal(t, testUserID, stepsIssue, "apply"), http.StatusUnprocessableEntity, evolution.ApplyFailed)
	var stepsAfter string
	dbfx.QueryRow(t, `SELECT steps::text FROM issue_workflow WHERE id = $1`, workflow.ID).Scan(&stepsAfter)
	if stepsAfter != stepsBefore {
		t.Fatalf("a refused steps change was written: %s", stepsAfter)
	}
	if n := countVersions(t, stepsIssue); n != 0 {
		t.Fatalf("a refused steps change wrote %d versions", n)
	}
}

// skillsFixture is a target agent with skill "review" bound and enabled, a
// second skill "tests", and a filed proposal that swaps review for tests.
type skillsFixture struct {
	proposalFixture
	review, tests, issueID string
}

func newSkillsFixture(t *testing.T) skillsFixture {
	t.Helper()
	sf := skillsFixture{proposalFixture: newProposalFixture(t, evolution.DefaultConfig())}
	// Short enough that no redaction rule takes it for a number to hide.
	suffix := workflowTestSuffix()
	suffix = suffix[len(suffix)-8:]
	skill := func(name string) string {
		id := dbfx.Insert(t, "skill", testutil.Cols{
			"workspace_id": testWorkspaceID, "name": name, "description": "evolution apply fixture",
			"content": "content", "config": testutil.Raw("'{}'::jsonb"), "created_by": testUserID,
		})
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_skill WHERE skill_id = $1`, id) })
		return id
	}
	reviewName, testsName := "evo-review-"+suffix, "evo-tests-"+suffix
	sf.review, sf.tests = skill(reviewName), skill(testsName)
	dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, sf.target, sf.review)

	body := sf.proposal()
	body["field"] = "skills"
	body["topic"] = "Reviewer lacks the tests skill"
	body["change"] = map[string]any{"before": reviewName, "after": testsName}
	sf.issueID = fileProposal(t, sf.proposalFixture, body)
	return sf
}

// binding returns whether the agent has the skill bound, and enabled.
func (sf skillsFixture) binding(t *testing.T, skillID string) (bound, enabled bool) {
	t.Helper()
	n := dbfx.Count(t, `SELECT count(*) FROM agent_skill WHERE agent_id = $1 AND skill_id = $2`, sf.target, skillID)
	e := dbfx.Count(t, `SELECT count(*) FROM agent_skill WHERE agent_id = $1 AND skill_id = $2 AND enabled`, sf.target, skillID)
	return n == 1, e == 1
}

// TestEvolutionApplySkills: the skills field sets the agent's enabled
// skills by name, and a rollback restores the previous set.
func TestEvolutionApplySkills(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sf := newSkillsFixture(t)
	changeProposal(t, testUserID, sf.issueID, "apply").Want(http.StatusOK)
	if _, tests := sf.binding(t, sf.tests); !tests {
		t.Fatal("after apply the tests skill should be enabled")
	}
	if bound, _ := sf.binding(t, sf.review); bound {
		t.Fatal("after apply the review skill should be unbound")
	}
	changeProposal(t, testUserID, sf.issueID, "rollback").Want(http.StatusOK)
	if bound, _ := sf.binding(t, sf.tests); bound {
		t.Fatal("after rollback the tests skill should be unbound")
	}
	if _, review := sf.binding(t, sf.review); !review {
		t.Fatal("after rollback the review skill should be enabled again")
	}
}

// holdSkillEdit starts the transaction of a person editing one of the
// agent's skill bindings, as SetAgentSkillEnabled does, and leaves it open.
func holdSkillEdit(t *testing.T, agentID, skillID string) (pgx.Tx, int) {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_skill SET enabled = false WHERE agent_id = $1 AND skill_id = $2`, agentID, skillID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("disable skill: %v (%d rows)", err, tag.RowsAffected())
	}
	return tx, pid
}

// changeProposalBehind runs apply or rollback while another transaction
// holds a lock it needs: it checks the call queues behind that transaction,
// commits it, and returns the call's response.
func changeProposalBehind(t *testing.T, holder pgx.Tx, holderPID int, issueID, action string) *testutil.Response {
	t.Helper()
	done := make(chan *testutil.Response, 1)
	go func() { done <- changeProposal(t, testUserID, issueID, action) }()
	if waitForWaiterPIDBlockedBy(t, holderPID, 5*time.Second) == 0 {
		t.Fatalf("%s never queued behind the open skill edit", action)
	}
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("commit the skill edit: %v", err)
	}
	select {
	case resp := <-done:
		return resp
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish after the skill edit committed", action)
		return nil
	}
}

// TestEvolutionApplyWaitsForSkillEdits forces the interleaving review R1
// named: a person disables the review skill while apply runs. Apply waits
// for that edit, sees the agent changed, and answers 409 without removing
// the binding; the person's edit stands.
func TestEvolutionApplyWaitsForSkillEdits(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	sf := newSkillsFixture(t)
	holder, pid := holdSkillEdit(t, sf.target, sf.review)
	wantRejected(t, changeProposalBehind(t, holder, pid, sf.issueID, "apply"), http.StatusConflict, evolution.ApplyStale)

	if bound, enabled := sf.binding(t, sf.review); !bound || enabled {
		t.Fatalf("the person's edit was overwritten: review bound %v enabled %v", bound, enabled)
	}
	if bound, _ := sf.binding(t, sf.tests); bound {
		t.Fatal("a refused apply bound the tests skill")
	}
	if n := countVersions(t, sf.issueID); n != 0 {
		t.Fatalf("a refused apply wrote %d versions", n)
	}
}

// TestEvolutionRollbackWaitsForSkillEdits is the same race for a rollback:
// a person disables the skill the apply enabled while the rollback runs.
func TestEvolutionRollbackWaitsForSkillEdits(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	sf := newSkillsFixture(t)
	changeProposal(t, testUserID, sf.issueID, "apply").Want(http.StatusOK)

	holder, pid := holdSkillEdit(t, sf.target, sf.tests)
	wantRejected(t, changeProposalBehind(t, holder, pid, sf.issueID, "rollback"), http.StatusConflict, evolution.ApplyChangedSinceApply)

	if bound, enabled := sf.binding(t, sf.tests); !bound || enabled {
		t.Fatalf("the person's edit was overwritten: tests bound %v enabled %v", bound, enabled)
	}
	if bound, _ := sf.binding(t, sf.review); bound {
		t.Fatal("a refused rollback bound the review skill again")
	}
	if n := countVersions(t, sf.issueID); n != 1 {
		t.Fatalf("%d version rows, want 1", n)
	}
}

// TestEvolutionProposalInPrivateProject: a proposal moved into a private
// project is not found for a member outside it, like the issue itself, and
// its writes answer that member 403 without loading it; an owner still sees
// and may apply it.
func TestEvolutionProposalInPrivateProject(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())
	private := dbfx.Project(t, "evolution apply private", testutil.Cols{"visibility": "private"})
	dbfx.Exec(t, `UPDATE issue SET project_id = $2 WHERE id = $1`, issueID, private)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE issue SET project_id = NULL WHERE id = $1`, issueID)
	})

	member := addSecondWorkspaceMember(t, "evolution-private-member@multica.test")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE user_id = $1`, member)
	})
	evolutionCall(t, member, newRequest("GET", proposalRoute(issueID, ""), nil)).Want(http.StatusNotFound)
	// Writes refuse a member before the issue is loaded, so they reveal
	// nothing either.
	changeProposal(t, member, issueID, "apply").Want(http.StatusForbidden)
	changeProposal(t, member, issueID, "rollback").Want(http.StatusForbidden)
	if state := proposalState(t, testUserID, issueID); !state.CanApply {
		t.Fatalf("owner state: %+v", state)
	}
}
