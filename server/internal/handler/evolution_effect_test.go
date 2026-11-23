// Added by the mouse9527/multica fork: Evolution P5 (M-139).

package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// appliedProposal files the fixture proposal, applies it, and backdates the
// apply past the default measurement window.
func appliedProposal(t *testing.T) (proposalFixture, string) {
	t.Helper()
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE evolution_config_version SET created_at = now() - interval '8 days'
		WHERE proposal_issue_id = $1 AND action = 'apply'`, issueID)
	return f, issueID
}

// targetRuns queues n finished runs of the target agent at the given age,
// each taking took; the first failed of them failed.
func targetRuns(t *testing.T, agentID, age, took string, n, failed int) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := "completed"
		if i < failed {
			status = "failed"
		}
		dbfx.Task(t, agentID, testutil.Cols{
			"status":       status,
			"created_at":   testutil.Raw("now() - interval '" + age + "'"),
			"started_at":   testutil.Raw("now() - interval '" + age + "'"),
			"completed_at": testutil.Raw("now() - interval '" + age + "' + interval '" + took + "'"),
		})
	}
}

type effectRow struct {
	verdict, reason     string
	sampleBefore, after int
	notified            bool
}

func evolutionEffect(t *testing.T, issueID string) (effectRow, int) {
	t.Helper()
	n := dbfx.Count(t, `SELECT count(*) FROM evolution_effect WHERE proposal_issue_id = $1`, issueID)
	var e effectRow
	if n > 0 {
		dbfx.QueryRow(t, `SELECT verdict, COALESCE(reason, ''), sample_before, sample_after, notified
			FROM evolution_effect WHERE proposal_issue_id = $1`, issueID).
			Scan(&e.verdict, &e.reason, &e.sampleBefore, &e.after, &e.notified)
	}
	return e, n
}

func effectAgentComments(t *testing.T, issueID, agentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2`, issueID, agentID)
}

func regressionInbox(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND type = $2 AND issue_id = $3 AND recipient_id = $4`,
		testWorkspaceID, evolution.RegressionInboxType, issueID, testUserID)
}

// TestEvolutionEffectSuggestsRollback: the target agent failed far more often
// in the week after the apply than in the week before. The tick records a
// regression, the Evolution agent comments "suggest rolling back" on the
// proposal, the owner gets an inbox item, and nothing is rolled back. A
// second tick measures nothing again.
func TestEvolutionEffectSuggestsRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f, issueID := appliedProposal(t)
	targetRuns(t, f.target, "12 days", "5 minutes", 10, 0)
	targetRuns(t, f.target, "4 days", "5 minutes", 10, 6)

	tickEvolution(t, nil)

	e, n := evolutionEffect(t, issueID)
	if n != 1 || e.verdict != evolution.EffectRegressed || !e.notified || e.sampleBefore != 10 || e.after != 10 {
		t.Fatalf("effect: %d rows, %+v", n, e)
	}
	if got := effectAgentComments(t, issueID, f.evo); got != 1 {
		t.Fatalf("Evolution left %d comments on the proposal", got)
	}
	var body string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2`, issueID, f.evo).Scan(&body)
	for _, want := range []string{"Suggest rolling back", "Failed runs", "0.0%", "60.0%", "Nothing was rolled back"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if got := regressionInbox(t, issueID); got != 1 {
		t.Errorf("owner inbox items: %d", got)
	}
	if got := proposalStateKey(t, issueID); got != evolution.StateApplied {
		t.Errorf("evolution_state = %q; a regression must not roll back", got)
	}
	if got := agentInstructions(t, f.target); got != proposalTargetApplied {
		t.Errorf("instructions changed: %q", got)
	}
	if got := countVersions(t, issueID); got != 1 {
		t.Errorf("%d version rows; nothing may be rolled back", got)
	}
	var effect string
	dbfx.QueryRow(t, `SELECT COALESCE(metadata->>'evolution_effect', '') FROM issue WHERE id = $1`, issueID).Scan(&effect)
	if effect != evolution.EffectRegressed {
		t.Errorf("evolution_effect = %q", effect)
	}

	tickEvolution(t, nil)
	if _, n := evolutionEffect(t, issueID); n != 1 {
		t.Errorf("measured again: %d rows", n)
	}
	if got := effectAgentComments(t, issueID, f.evo); got != 1 {
		t.Errorf("commented again: %d", got)
	}
}

// TestEvolutionEffectBelowSampleDrawsNoConclusion: with fewer runs than the
// minimum in a window, the verdict is insufficient_data however the numbers
// moved, and nobody is told anything.
func TestEvolutionEffectBelowSampleDrawsNoConclusion(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f, issueID := appliedProposal(t)
	targetRuns(t, f.target, "12 days", "5 minutes", 10, 0)
	targetRuns(t, f.target, "4 days", "5 minutes", evolution.DefaultMinEffectSample-1, evolution.DefaultMinEffectSample-1)

	tickEvolution(t, nil)

	e, n := evolutionEffect(t, issueID)
	if n != 1 || e.verdict != evolution.EffectInsufficient || e.notified {
		t.Fatalf("effect: %d rows, %+v", n, e)
	}
	if got := effectAgentComments(t, issueID, f.evo); got != 0 {
		t.Errorf("Evolution commented %d times without enough data", got)
	}
	if got := regressionInbox(t, issueID); got != 0 {
		t.Errorf("owner told %d times without enough data", got)
	}
}

// TestEvolutionEffectJudgesEachMetricOnItsOwnSample: ten runs in each
// window, nine of them failed both times, and the one completed run took 60
// seconds before and 600 after. The run count passes the minimum, but the
// duration rests on one run per window, so it is not judged; the failure
// rate did not move, so nothing is suggested.
func TestEvolutionEffectJudgesEachMetricOnItsOwnSample(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f, issueID := appliedProposal(t)
	targetRuns(t, f.target, "12 days", "60 seconds", 10, 9)
	targetRuns(t, f.target, "4 days", "600 seconds", 10, 9)

	tickEvolution(t, nil)

	e, n := evolutionEffect(t, issueID)
	if n != 1 || e.verdict != evolution.EffectUnchanged || e.notified {
		t.Fatalf("effect: %d rows, %+v", n, e)
	}
	var judged int
	dbfx.QueryRow(t, `SELECT count(*) FROM evolution_effect, jsonb_array_elements(findings) f
		WHERE proposal_issue_id = $1 AND f.value->>'metric' = $2`, issueID, evolution.MetricAvgDurationSecs).Scan(&judged)
	if judged != 0 {
		t.Error("the duration of one run per window was judged")
	}
	if got := effectAgentComments(t, issueID, f.evo); got != 0 {
		t.Errorf("Evolution commented %d times", got)
	}
	if got := regressionInbox(t, issueID); got != 0 {
		t.Errorf("owner told %d times", got)
	}
}

// TestEvolutionEffectNotMeasuredAfterRollback: an apply rolled back inside
// its measurement window is recorded as not measured and never suggested for
// rollback.
func TestEvolutionEffectNotMeasuredAfterRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f, issueID := appliedProposal(t)
	changeProposal(t, testUserID, issueID, "rollback").Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE evolution_config_version SET created_at = now() - interval '6 days'
		WHERE proposal_issue_id = $1 AND action = 'rollback'`, issueID)
	targetRuns(t, f.target, "12 days", "5 minutes", 10, 0)
	targetRuns(t, f.target, "4 days", "5 minutes", 10, 10)

	tickEvolution(t, nil)

	e, n := evolutionEffect(t, issueID)
	if n != 1 || e.verdict != evolution.EffectNotMeasured || e.reason != evolution.NotMeasuredRolledBack || e.notified {
		t.Fatalf("effect: %d rows, %+v", n, e)
	}
	if got := effectAgentComments(t, issueID, f.evo); got != 0 {
		t.Errorf("Evolution commented %d times on a rolled-back proposal", got)
	}
}

// TestEvolutionEffectWaitsForTheWindow: an apply younger than the window is
// not measured yet.
func TestEvolutionEffectWaitsForTheWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	issueID := fileProposal(t, f, f.proposal())
	changeProposal(t, testUserID, issueID, "apply").Want(http.StatusOK)

	tickEvolution(t, nil)

	if _, n := evolutionEffect(t, issueID); n != 0 {
		t.Fatalf("measured a fresh apply: %d rows", n)
	}
}
