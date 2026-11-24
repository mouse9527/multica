// Added by the mouse9527/multica fork: the project Evolution files its issues
// into (M-231).

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// issueProject is the issue's project id, or "" for none.
func issueProject(t *testing.T, issueID string) string {
	t.Helper()
	var project string
	dbfx.QueryRow(t, `SELECT COALESCE(project_id::text, '') FROM issue WHERE id = $1`, issueID).Scan(&project)
	return project
}

func openRunIssue(t *testing.T) string {
	t.Helper()
	var issueID string
	dbfx.QueryRow(t, `SELECT issue_id::text FROM evolution_run WHERE workspace_id = $1 AND status = 'open'`,
		testWorkspaceID).Scan(&issueID)
	return issueID
}

// TestEvolutionOutputProjectHoldsEveryIssue: with output_project_id set, the
// analysis issue and the proposals it files go into that project.
func TestEvolutionOutputProjectHoldsEveryIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	project := dbfx.Project(t, "evolution output project")
	cfg := evolution.DefaultConfig()
	cfg.OutputProjectID = project
	f := newProposalFixture(t, cfg)

	if got := issueProject(t, f.runIssueID); got != project {
		t.Errorf("analysis issue project %q, want %q", got, project)
	}
	var res evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&res)
	if got := issueProject(t, res.IssueID); got != project {
		t.Errorf("proposal issue project %q, want %q", got, project)
	}

	var st evolutionConfigResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution/config", nil)).Want(http.StatusOK).JSON(&st)
	if st.Config.OutputProjectID != project {
		t.Errorf("config output_project_id %q, want %q", st.Config.OutputProjectID, project)
	}
}

// TestEvolutionOutputProjectHoldsTheCondenseReview: the monthly condense
// review issue goes into the output project too (the newCondenseFixture setup
// plus output_project_id).
func TestEvolutionOutputProjectHoldsTheCondenseReview(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableEvolutionAgent(t)
	cleanupProposalIssues(t)
	visibleAgent(t, "evolution output project condense target", longInstructions(20))
	project := dbfx.Project(t, "evolution output project condense")
	cfg := evolution.DefaultConfig()
	cfg.Enabled = true
	cfg.Schedule.Time = "00:00"
	cfg.CondenseDayOfMonth = 1
	cfg.CondenseMinRunes = evolution.MinCondenseMinRunes
	cfg.OutputProjectID = project
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	tickEvolution(t, nil)

	var reviewIssue string
	dbfx.QueryRow(t, `SELECT issue_id::text FROM evolution_review WHERE workspace_id = $1 AND status = 'open'`,
		testWorkspaceID).Scan(&reviewIssue)
	if got := issueProject(t, reviewIssue); got != project {
		t.Errorf("condense review issue project %q, want %q", got, project)
	}
}

// TestEvolutionNoOutputProjectFilesWithoutProject: without the setting,
// Evolution's issues have no project, as before.
func TestEvolutionNoOutputProjectFilesWithoutProject(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newProposalFixture(t, evolution.DefaultConfig())
	if got := issueProject(t, f.runIssueID); got != "" {
		t.Errorf("analysis issue project %q, want none", got)
	}
	var res evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&res)
	if got := issueProject(t, res.IssueID); got != "" {
		t.Errorf("proposal issue project %q, want none", got)
	}
}

// outputProjectGone are the ways a configured output project stops being
// usable after it was saved.
var outputProjectGone = map[string]string{
	"deleted":  `DELETE FROM project WHERE id = $1`,
	"archived": `UPDATE project SET archived_at = now() WHERE id = $1`,
	"private":  `UPDATE project SET visibility = 'private' WHERE id = $1`,
}

// TestEvolutionOutputProjectGoneFallsBackToNone: a project deleted, archived,
// or made private after it was configured stops neither a run nor a
// proposal; the issue is filed without a project.
func TestEvolutionOutputProjectGoneFallsBackToNone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, gone := range outputProjectGone {
		t.Run("run/"+name, func(t *testing.T) {
			enableEvolutionAgent(t)
			project := dbfx.Project(t, "evolution output project run "+name)
			cfg := evolutionTestConfig()
			cfg.OutputProjectID = project
			putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
			dbfx.Exec(t, gone, project)
			backdateEvolutionCursors(t)
			tickEvolution(t, nil)
			if got := issueProject(t, openRunIssue(t)); got != "" {
				t.Errorf("analysis issue project %q, want none", got)
			}
		})
		t.Run("proposal/"+name, func(t *testing.T) {
			project := dbfx.Project(t, "evolution output project proposal "+name)
			cfg := evolution.DefaultConfig()
			cfg.OutputProjectID = project
			f := newProposalFixture(t, cfg)
			dbfx.Exec(t, gone, project)
			var res evolution.FileResult
			propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&res)
			if got := issueProject(t, res.IssueID); got != "" {
				t.Errorf("proposal issue project %q, want none", got)
			}
		})
	}
}

// TestEvolutionOutputProjectMustBeUsable: saving refuses a project that is not
// in the workspace, is private, or is archived, and an id that is not a uuid.
func TestEvolutionOutputProjectMustBeUsable(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableEvolutionAgent(t)
	private := dbfx.Project(t, "evolution output project private", testutil.Cols{"visibility": "private"})
	archived := dbfx.Project(t, "evolution output project archived")
	dbfx.Exec(t, `UPDATE project SET archived_at = now() WHERE id = $1`, archived)
	for name, id := range map[string]string{
		"missing":  "00000000-0000-4000-8000-000000000231",
		"private":  private,
		"archived": archived,
		"not uuid": "evolution",
	} {
		cfg := evolutionTestConfig()
		cfg.OutputProjectID = id
		if resp := putEvolutionConfig(t, cfg, false); resp.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.Code)
		}
	}
	var st evolutionConfigResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution/config", nil)).Want(http.StatusOK).JSON(&st)
	if st.Config.OutputProjectID != "" {
		t.Fatalf("a refused project was stored: %q", st.Config.OutputProjectID)
	}

	// Clearing it is always allowed.
	public := dbfx.Project(t, "evolution output project public")
	cfg := evolutionTestConfig()
	cfg.OutputProjectID = public
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	cfg.OutputProjectID = ""
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution/config", nil)).Want(http.StatusOK).JSON(&st)
	if st.Config.OutputProjectID != "" {
		t.Fatalf("output_project_id %q after clearing", st.Config.OutputProjectID)
	}
}
