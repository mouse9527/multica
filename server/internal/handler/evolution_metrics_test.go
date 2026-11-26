// Added by the mouse9527/multica fork: Evolution structural analysis, the
// metrics source and the weekly structural review (M-296).

package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func metricsRead(t *testing.T, agentID, taskID, days string) *testutil.Response {
	t.Helper()
	q := url.Values{"source": {evolution.SourceMetrics}}
	if days != "" {
		q.Set("days", days)
	}
	req := newRequest("GET", "/api/evolution/signals?"+q.Encode(), nil)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	return evolutionCall(t, testUserID, req)
}

// TestEvolutionMetricsSource: the metrics source is off until the workspace
// turns it on; then the week's structural review run reads one summary of
// its window, and any other run is refused.
// The summary counts a workflow's returns and lists the return loop, counts
// the visible agents' runs and handoffs, and leaves out private projects,
// private agents, runs before the window, and the Evolution agent itself.
// Assertions stay on this test's own workflow and agents: other tests write
// to the same workspace.
func TestEvolutionMetricsSource(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	evo := enableEvolutionAgent(t)
	cfg := evolutionTestConfig()
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	backdateEvolutionCursors(t)
	tickEvolution(t, nil)
	runID, taskID := openEvolutionRunTask(t, evo)

	metricsRead(t, evo, taskID, "").Want(http.StatusConflict)
	cfg.Sources.Metrics = true
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	metricsRead(t, evo, taskID, "40").Want(http.StatusBadRequest)

	workflow := dbfx.Insert(t, "issue_workflow", testutil.Cols{
		"workspace_id": testWorkspaceID, "name": "evolution metrics " + workflowTestSuffix(), "initial_status_key": "todo",
		"steps": `[{"status_key":"todo"},{"status_key":"in_progress"},{"status_key":"in_review","back_status_key":"todo"},{"status_key":"done"}]`,
	})
	public := dbfx.Project(t, "evolution metrics public", testutil.Cols{"workflow_id": workflow})
	private := dbfx.Project(t, "evolution metrics private", testutil.Cols{"workflow_id": workflow, "visibility": "private"})
	loop := dbfx.Issue(t, "evolution metrics loop", testutil.Cols{"project_id": public, "created_at": testutil.Raw("now() - interval '5 days'")})
	hidden := dbfx.Issue(t, "evolution metrics hidden", testutil.Cols{"project_id": private, "created_at": testutil.Raw("now() - interval '5 days'")})
	moved := func(issueID, from, to string, hoursAgo int) {
		dbfx.Insert(t, "activity_log", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": issueID, "actor_type": "member", "actor_id": testUserID,
			"action": "status_changed", "details": fmt.Sprintf(`{"from":%q,"to":%q}`, from, to),
			"created_at": testutil.Raw(fmt.Sprintf("now() - interval '%d hours'", hoursAgo)),
		})
	}
	hour := 60
	for i := 0; i < 3; i++ {
		moved(loop, "todo", "in_review", hour)
		moved(loop, "in_review", "todo", hour-2)
		hour -= 4
	}
	moved(hidden, "todo", "in_review", 50)
	moved(hidden, "in_review", "todo", 48)

	runtimeID := handlerTestRuntimeID(t)
	visible := func(name string) string {
		id := dbfx.Agent(t, name, runtimeID, testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
		dbfx.Insert(t, "agent_invocation_target", testutil.Cols{
			"agent_id": id, "target_type": "workspace", "target_id": testWorkspaceID,
		})
		return id
	}
	worker := visible("evolution metrics worker")
	reviewer := visible("evolution metrics reviewer")
	privateAgent := dbfx.Agent(t, "evolution metrics private agent", runtimeID)
	run := func(agentID, issueID string, daysAgo int, took string) {
		at := fmt.Sprintf("now() - interval '%d days'", daysAgo)
		cols := testutil.Cols{
			"status":       "completed",
			"created_at":   testutil.Raw(at),
			"started_at":   testutil.Raw(at),
			"completed_at": testutil.Raw(at + " + interval '" + took + "'"),
		}
		if issueID != "" {
			cols["issue_id"] = issueID
		}
		dbfx.Task(t, agentID, cols)
	}
	run(worker, loop, 1, "30 seconds")
	run(worker, "", 2, "10 minutes")
	run(worker, hidden, 1, "1 minute")
	run(worker, loop, 20, "1 minute")
	run(privateAgent, loop, 1, "1 minute")
	// Assigned to the worker by hand, then handed to the reviewer by entering
	// a workflow step: that handoff rides on the status row and names no
	// "from" while the Router's annotation is off.
	dbfx.Insert(t, "activity_log", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": loop, "actor_type": "member", "actor_id": testUserID,
		"action":     "assignee_changed",
		"details":    fmt.Sprintf(`{"to_type":"agent","to_id":%q}`, worker),
		"created_at": testutil.Raw("now() - interval '2 hours'"),
	})
	dbfx.Insert(t, "activity_log", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": loop, "actor_type": "member", "actor_id": testUserID,
		"action":     "status_changed",
		"details":    fmt.Sprintf(`{"from":"todo","to":"in_review","handoff_workflow":"delivery","handoff_to_type":"agent","handoff_to_id":%q}`, reviewer),
		"created_at": testutil.Raw("now() - interval '1 hour'"),
	})

	// Only the week's structural review reads metrics: with a scheduled run
	// earlier in the same week that did not fail, this run is a daily one.
	dbfx.Exec(t, `UPDATE evolution_run SET slot = date_trunc('week', current_date)::date + 1 WHERE id = $1`, runID)
	monday := dbfx.Insert(t, "evolution_run", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": evo, "kind": "scheduled", "status": "succeeded",
		"slot": testutil.Raw("date_trunc('week', current_date)::date"), "window_until": testutil.Raw("now() - interval '1 day'"),
	})
	metricsRead(t, evo, taskID, "").Want(http.StatusConflict)
	// A failed run does not count: the next scheduled run takes its place.
	dbfx.Exec(t, `UPDATE evolution_run SET status = 'failed', failure_reason = 'test' WHERE id = $1`, monday)

	var got evolution.MetricsSummary
	metricsRead(t, evo, taskID, "").Want(http.StatusOK).JSON(&got)
	if got.Source != evolution.SourceMetrics || got.WindowDays != evolution.DefaultMetricsWindowDays {
		t.Fatalf("summary header = %+v", got)
	}

	var wf *evolution.WorkflowSummary
	for i := range got.Workflows {
		if got.Workflows[i].ID == workflow {
			wf = &got.Workflows[i]
		}
	}
	if wf == nil {
		t.Fatalf("workflow %s missing from %+v", workflow, got.Workflows)
	}
	if wf.Issues != 1 {
		t.Fatalf("the private project's issue was counted: %+v", wf)
	}
	foundReview := false
	for _, s := range wf.Steps {
		if s.StatusKey != "in_review" {
			continue
		}
		foundReview = true
		if s.Exits != 3 || s.Returns != 3 || s.ReturnRate == nil || *s.ReturnRate != 1 {
			t.Fatalf("in_review = %+v", s)
		}
	}
	if !foundReview {
		t.Fatalf("in_review missing from %+v", wf.Steps)
	}
	foundLoop := false
	for _, l := range got.ReturnLoops {
		if l.IssueID == hidden {
			t.Fatal("an issue in a private project is listed")
		}
		if l.IssueID == loop {
			foundLoop = l.Returns == 3 && l.Identifier != "" && l.WorkflowID == workflow
		}
	}
	if !foundLoop {
		t.Fatalf("return loop missing: %+v", got.ReturnLoops)
	}

	agents := map[string]evolution.AgentSummary{}
	for _, a := range got.Agents {
		agents[a.ID] = a
	}
	for name, id := range map[string]string{"private agent": privateAgent, "Evolution": evo} {
		if _, ok := agents[id]; ok {
			t.Errorf("%s must not be summarised", name)
		}
	}
	w, ok := agents[worker]
	if !ok {
		t.Fatalf("worker missing from %+v", got.Agents)
	}
	// In the window and outside private projects: the run on loop and the
	// run without an issue.
	if w.Runs != 2 || w.Completed != 2 || w.ShortRuns != 1 || w.HandoffsIn != 1 || w.HandoffsOut != 1 {
		t.Fatalf("worker = %+v", w)
	}
	if r := agents[reviewer]; r.Runs != 0 || r.HandoffsIn != 1 {
		t.Fatalf("reviewer = %+v", r)
	}
}

// openStructureRunIssue is the title and description of the open run's
// analysis issue.
func openStructureRunIssue(t *testing.T) (title, description string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT i.title, COALESCE(i.description, '') FROM evolution_run r JOIN issue i ON i.id = r.issue_id
		WHERE r.workspace_id = $1 AND r.status = 'open'`, testWorkspaceID).Scan(&title, &description)
	return title, description
}

// TestEvolutionStructuralReviewRun: with the metrics source on, the week's
// first scheduled run is the structural review; its issue says so in the
// title and lists the metrics source. With it off, the same run is a plain
// daily analysis that never mentions metrics.
func TestEvolutionStructuralReviewRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, metrics := range []bool{true, false} {
		t.Run(fmt.Sprintf("metrics=%v", metrics), func(t *testing.T) {
			enableEvolutionAgent(t)
			cfg := evolutionTestConfig()
			cfg.Sources.Metrics = metrics
			putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
			backdateEvolutionCursors(t)
			tickEvolution(t, nil)

			title, description := openStructureRunIssue(t)
			if metrics {
				if !strings.HasSuffix(title, " · structural review") ||
					!strings.Contains(description, "--source metrics --days 14") {
					t.Fatalf("structural review issue = %q\n%s", title, description)
				}
				return
			}
			if strings.Contains(title, "structural") || strings.Contains(description, "metrics") {
				t.Fatalf("daily issue mentions the structural review = %q\n%s", title, description)
			}
		})
	}
}
