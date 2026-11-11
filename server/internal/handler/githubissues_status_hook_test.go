// Modified for the mouse9527/multica fork: GitHub-native issues.

package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// gitHubStatusHandler is the shared test handler with the fork's status
// hooks on, as the router wires them while the feature is on.
func gitHubStatusHandler(t *testing.T) *Handler {
	t.Helper()
	svc := githubissues.New(githubissues.LoadConfig(func(key string) string {
		return map[string]string{
			githubissues.EnvEnabled:        "true",
			githubissues.EnvToken:          fakeGitHubToken,
			githubissues.EnvTokenExpiresAt: "never",
		}[key]
	}), githubissues.Options{DB: testPool})
	h := *testHandler
	h.GitHubStatuses = svc
	return &h
}

// markGitHubIssue makes a fixture issue GitHub-backed.
func markGitHubIssue(t *testing.T, issueID string, number int) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO issue_external (issue_id, workspace_id, provider, external_repo_id, external_repo,
		external_number, external_node_id, state, external_updated_at)
		VALUES ($1, $2, 'github', 42, 'acme/widgets', $3, 'I_test', 'open', now())`, issueID, testWorkspaceID, number)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_external WHERE issue_id = $1`, issueID)
	})
}

func setStatusLabelMap(t *testing.T, projectID, labelMap string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO project_github_settings (project_id, workspace_id, status_label_map) VALUES ($1, $2, $3)`,
		projectID, testWorkspaceID, labelMap)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project_github_settings WHERE project_id = $1`, projectID)
	})
}

func loadIssueRow(t *testing.T, issueID string) db.Issue {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

// A status change that arrives from GitHub runs UpdateIssue, so entering a
// step with a handler hands the issue off and starts the handler's run, as
// the same change made in Multica does.
func TestGitHubStatusChangeHandsOff(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	projectID := deliveryProject(t, "GitHub status handoff "+workflowTestSuffix(), agentID)
	setStatusLabelMap(t, projectID, `{"in_progress":"status:in-progress"}`)
	issueID := dbfx.Issue(t, "labelled on GitHub", testutil.Cols{"project_id": projectID, "status": "backlog"})
	markGitHubIssue(t, issueID, int(time.Now().UnixNano()%1_000_000))
	h := gitHubStatusHandler(t)

	if err := h.WriteGitHubStatus(context.Background(), util.MustParseUUID(testUserID), loadIssueRow(t, issueID), "in_progress"); err != nil {
		t.Fatalf("write status: %v", err)
	}
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, issueID)
	if status != "in_progress" || assigneeType != "agent" || assigneeID != agentID {
		t.Fatalf("issue = %s %s/%s, want in_progress handed to agent %s", status, assigneeType, assigneeID, agentID)
	}
	tasks, err := testHandler.Queries.ListTasksByIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %d (%v), want the handoff run", len(tasks), err)
	}

	// Closed on GitHub: done.
	if err := h.WriteGitHubStatus(context.Background(), util.MustParseUUID(testUserID), loadIssueRow(t, issueID), "done"); err != nil {
		t.Fatalf("write done: %v", err)
	}
	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "done" {
		t.Fatalf("status = %s, want done", status)
	}
}

// A status the project's workflow does not list comes back as a refusal the
// poller does not retry.
func TestGitHubStatusWriteRefusedByWorkflow(t *testing.T) {
	seedTestCatalog(t)
	projectID := createWorkflowTestProject(t, "GitHub status refused "+workflowTestSuffix())
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": createTestWorkflow(t, "Narrow "+workflowTestSuffix()).ID}).Want(http.StatusOK)
	setStatusLabelMap(t, projectID, `{"in_review":"status:review"}`)
	issueID := dbfx.Issue(t, "outside the workflow", testutil.Cols{"project_id": projectID, "status": "backlog"})
	markGitHubIssue(t, issueID, int(time.Now().UnixNano()%1_000_000))

	err := gitHubStatusHandler(t).WriteGitHubStatus(context.Background(), util.MustParseUUID(testUserID), loadIssueRow(t, issueID), "in_review")
	var refused *githubissues.StatusWriteError
	if !errors.As(err, &refused) || refused.Code != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 refusal", err)
	}
	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "backlog" {
		t.Fatalf("status = %s, want backlog kept", status)
	}
}

// Setting a GitHub-backed issue to a status GitHub cannot show is refused
// with the statuses that can be shown; a mapped status, a local issue, and
// the feature off all pass as before.
func TestUpdateIssueRejectsUnmappedGitHubStatus(t *testing.T) {
	seedTestCatalog(t)
	projectID := createWorkflowTestProject(t, "GitHub status map "+workflowTestSuffix())
	setStatusLabelMap(t, projectID, `{"in_progress":"status:in-progress"}`)
	gh := dbfx.Issue(t, "github issue", testutil.Cols{"project_id": projectID, "status": "backlog"})
	markGitHubIssue(t, gh, int(time.Now().UnixNano()%1_000_000))
	local := dbfx.Issue(t, "local issue", testutil.Cols{"project_id": projectID, "status": "backlog"})
	h := gitHubStatusHandler(t)

	update := func(handler *Handler, issueID, status string) *testutil.Response {
		req := withURLParam(newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": status}), "id", issueID)
		return testutil.Call(t, handler.UpdateIssue, req)
	}
	var refused struct {
		Code    string   `json:"code"`
		Allowed []string `json:"allowed_statuses"`
	}
	update(h, gh, "in_review").Want(http.StatusUnprocessableEntity).JSON(&refused)
	if refused.Code != "github_status_unmapped" || len(refused.Allowed) != 2 {
		t.Fatalf("refusal = %+v", refused)
	}
	if status, _, _ := issueStatusAndAssignee(t, gh); status != "backlog" {
		t.Fatalf("refused write changed the status to %s", status)
	}
	update(h, gh, "in_progress").Want(http.StatusOK)
	update(h, gh, "done").Want(http.StatusOK)
	update(h, local, "in_review").Want(http.StatusOK)

	// A batch skips the GitHub issue and updates the local one.
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog' WHERE id = ANY($1::uuid[])`, []string{gh, local})
	req := newRequest("PATCH", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{gh, local},
		"updates":   map[string]any{"status": "blocked"},
	})
	testutil.Call(t, h.BatchUpdateIssues, req).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, gh); status != "backlog" {
		t.Fatalf("batch moved the GitHub issue to %s", status)
	}
	if status, _, _ := issueStatusAndAssignee(t, local); status != "blocked" {
		t.Fatalf("batch left the local issue on %s", status)
	}

	// Feature off: no hook, upstream behavior.
	if testHandler.GitHubStatuses != nil {
		t.Fatal("the shared test handler must not carry the fork hook")
	}
	update(testHandler, gh, "in_review").Want(http.StatusOK)
}
