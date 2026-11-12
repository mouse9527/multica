// Added by the mouse9527/multica fork: configuring a project's status labels
// for GitHub-native issues. Upstream has no such file.

package handler

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func putGitHubSettings(t *testing.T, h *Handler, projectID string, body any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, h.SetProjectGitHubSettings, withURLParam(newRequest(http.MethodPut,
		"/api/projects/"+projectID+"/github-settings", body), "id", projectID))
}

func getGitHubSettings(t *testing.T, h *Handler, projectID string) map[string]any {
	t.Helper()
	return testutil.Call(t, h.GetProjectGitHubSettings, withURLParam(newRequest(http.MethodGet,
		"/api/projects/"+projectID+"/github-settings", nil), "id", projectID)).Want(http.StatusOK).Map()
}

func anyStrings(v any) []string {
	out := []string{}
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}

func labelMapOf(t *testing.T, settings map[string]any) map[string]string {
	t.Helper()
	raw, ok := settings["status_label_map"].(map[string]any)
	if !ok {
		t.Fatalf("status_label_map = %#v, want an object", settings["status_label_map"])
	}
	out := map[string]string{}
	for k, v := range raw {
		out[k] = v.(string)
	}
	return out
}

// PUT validates the status label map against the project's workflow and
// saves it together with the default repository, or saves neither; a body
// without status_label_map behaves as before.
func TestProjectGitHubSettingsStatusLabelMap(t *testing.T) {
	seedTestCatalog(t)
	h, _, _ := newGitHubWriteHandler(t)
	projectID := createWorkflowTestProject(t, "GitHub status labels "+workflowTestSuffix())
	setProjectWorkflow(t, projectID, map[string]any{
		"workflow_id": createTestWorkflow(t, "Labels "+workflowTestSuffix(), "in_progress", "in_review").ID,
	}).Want(http.StatusOK)
	widgets := githubRepoResource(t, projectID, "acme/widgets")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project_github_settings WHERE project_id = $1`, projectID)
	})

	got := getGitHubSettings(t, h, projectID)
	mappable := anyStrings(got["mappable_statuses"])
	if slices.Contains(mappable, "backlog") || slices.Contains(mappable, "done") || slices.Contains(mappable, "cancelled") ||
		!slices.Contains(mappable, "in_progress") || !slices.Contains(mappable, "in_review") || !slices.Contains(mappable, "blocked") {
		t.Fatalf("mappable_statuses = %v, want the workflow's statuses without entry, done and cancelled", mappable)
	}
	if len(labelMapOf(t, got)) != 0 {
		t.Fatalf("status_label_map = %v, want {}", got["status_label_map"])
	}

	// Map and default together.
	saved := putGitHubSettings(t, h, projectID, map[string]any{
		"default_resource_id": widgets,
		"status_label_map":    map[string]any{"in_progress": " status:in-progress ", "in_review": "Status:Review"},
	}).Want(http.StatusOK).Map()
	want := map[string]string{"in_progress": "status:in-progress", "in_review": "Status:Review"}
	if !mapsEqual(labelMapOf(t, saved), want) || saved["default_resource_id"] != widgets {
		t.Fatalf("saved = %v", saved)
	}
	if statuses := anyStrings(saved["create_statuses"]); !slices.Equal(statuses, []string{"backlog", "in_progress", "in_review"}) {
		t.Fatalf("create_statuses = %v", statuses)
	}

	t.Run("refused maps change nothing", func(t *testing.T) {
		for _, tc := range []struct {
			labels map[string]any
			code   string
		}{
			{map[string]any{"started_9": "status:nine"}, "github_status_label_key_invalid"},
			{map[string]any{"backlog": "status:backlog"}, "github_status_label_key_invalid"},
			{map[string]any{"done": "status:done"}, "github_status_label_key_invalid"},
			{map[string]any{"cancelled": "status:cancelled"}, "github_status_label_key_invalid"},
			{map[string]any{"in_review": "review"}, "github_status_label_invalid"},
			{map[string]any{"in_review": "status:"}, "github_status_label_invalid"},
			{map[string]any{"in_review": "status:   "}, "github_status_label_invalid"},
			{map[string]any{"in_review": "status:" + strings.Repeat("x", 50)}, "github_status_label_invalid"},
			{map[string]any{"in_progress": "status:Review", "in_review": "status:review"}, "github_status_label_duplicate"},
		} {
			body := putGitHubSettings(t, h, projectID, map[string]any{
				"default_resource_id": nil, "status_label_map": tc.labels,
			}).Want(http.StatusBadRequest).Map()
			if body["code"] != tc.code || len(anyStrings(body["mappable_statuses"])) == 0 || body["error"] == "" {
				t.Fatalf("%v: body = %v, want code %s with the mappable statuses", tc.labels, body, tc.code)
			}
			if tc.code == "github_status_label_key_invalid" && !strings.Contains(body["error"].(string), "in_progress") {
				t.Fatalf("%v: error %q does not name the mappable statuses", tc.labels, body["error"])
			}
			kept := getGitHubSettings(t, h, projectID)
			if !mapsEqual(labelMapOf(t, kept), want) || kept["default_resource_id"] != widgets {
				t.Fatalf("%v: refused PUT changed the settings to %v", tc.labels, kept)
			}
		}
		putGitHubSettings(t, h, projectID, map[string]any{"status_label_map": []string{"status:x"}}).Want(http.StatusBadRequest)
		putGitHubSettings(t, h, projectID, map[string]any{"status_label_map": map[string]any{"in_review": 7}}).Want(http.StatusBadRequest)
	})

	t.Run("omitted parts are kept", func(t *testing.T) {
		// The request as it was before status_label_map existed.
		saved := putGitHubSettings(t, h, projectID, map[string]any{"default_resource_id": nil}).Want(http.StatusOK).Map()
		if saved["default_resource_id"] != nil || !mapsEqual(labelMapOf(t, saved), want) {
			t.Fatalf("default-only PUT = %v", saved)
		}
		putGitHubSettings(t, h, projectID, map[string]any{"default_resource_id": widgets}).Want(http.StatusOK)
		saved = putGitHubSettings(t, h, projectID, map[string]any{
			"status_label_map": map[string]any{"blocked": "status:blocked"},
		}).Want(http.StatusOK).Map()
		if saved["default_resource_id"] != widgets || !mapsEqual(labelMapOf(t, saved), map[string]string{"blocked": "status:blocked"}) {
			t.Fatalf("labels-only PUT = %v", saved)
		}
	})

	t.Run("empty or null clears the map", func(t *testing.T) {
		for _, clear := range []any{map[string]any{}, nil} {
			putGitHubSettings(t, h, projectID, map[string]any{"status_label_map": map[string]any{"blocked": "status:blocked"}}).Want(http.StatusOK)
			saved := putGitHubSettings(t, h, projectID, map[string]any{"status_label_map": clear}).Want(http.StatusOK).Map()
			if len(labelMapOf(t, saved)) != 0 || saved["default_resource_id"] != widgets {
				t.Fatalf("clearing with %v = %v", clear, saved)
			}
		}
		// A body with neither field still clears the default, as before.
		saved := putGitHubSettings(t, h, projectID, map[string]any{}).Want(http.StatusOK).Map()
		if saved["default_resource_id"] != nil {
			t.Fatalf("empty PUT = %v", saved)
		}
	})
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// After the project maps its statuses, a GitHub issue created with a
// repository moves through each mapped status, GitHub's labels follow, and a
// label changed on GitHub moves the issue back; clearing the map brings back
// the refusal.
func TestGitHubIssueMovesThroughConfiguredStatusLabels(t *testing.T) {
	seedTestCatalog(t)
	h, fake, _ := newGitHubWriteHandler(t)
	h.GitHubStatuses = h.GitHubIssues
	h.IssueViews = h.GitHubIssues
	h.GitHubIssues.SetStatusWriter(h)
	projectID := createWorkflowTestProject(t, "GitHub status flow "+workflowTestSuffix())
	setProjectWorkflow(t, projectID, map[string]any{
		"workflow_id": createTestWorkflow(t, "Flow "+workflowTestSuffix(), "in_progress", "in_review").ID,
	}).Want(http.StatusOK)
	githubRepoResource(t, projectID, "acme/widgets")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project_github_settings WHERE project_id = $1`, projectID)
	})

	var issue map[string]any
	testutil.Call(t, h.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title": "Through the workflow", "repo": "acme/widgets", "project_id": projectID, "status": "backlog",
	})).Want(http.StatusCreated).JSON(&issue)
	issueID := issue["id"].(string)
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })
	var number int
	dbfx.QueryRow(t, `SELECT external_number FROM issue_external WHERE issue_id = $1`, issueID).Scan(&number)
	fake.take()

	update := func(status string) *testutil.Response {
		return testutil.Call(t, h.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID,
			map[string]any{"status": status}), "id", issueID))
	}
	status := func() string {
		s, _, _ := issueStatusAndAssignee(t, issueID)
		return s
	}

	// Unmapped: refused, as before.
	update("in_progress").Want(http.StatusUnprocessableEntity)

	labels := map[string]any{"in_progress": "status:in-progress", "in_review": "status:review", "blocked": "status:blocked"}
	putGitHubSettings(t, h, projectID, map[string]any{"status_label_map": labels}).Want(http.StatusOK)
	fake.relabel(number, "bug")
	for _, step := range []struct{ status, label string }{
		{"in_progress", "status:in-progress"},
		{"in_review", "status:review"},
		{"blocked", "status:blocked"},
		{"backlog", ""},
	} {
		update(step.status).Want(http.StatusOK)
		want := []string{"bug"}
		if step.label != "" {
			want = append(want, step.label)
		}
		if got := fake.labels(number); !slices.Equal(got, want) || status() != step.status {
			t.Fatalf("after %s: GitHub labels %v (want %v), status %s", step.status, got, want, status())
		}
	}

	// A label changed on GitHub comes back as the status (on-view refresh).
	fake.relabel(number, "bug", "status:review")
	testutil.Call(t, h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID, nil), "id", issueID)).
		Want(http.StatusOK)
	for deadline := time.Now().Add(5 * time.Second); status() != "in_review"; {
		if time.Now().After(deadline) {
			t.Fatalf("status = %s after the label changed on GitHub, want in_review", status())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Cleared: back to the refusal.
	putGitHubSettings(t, h, projectID, map[string]any{"status_label_map": map[string]any{}}).Want(http.StatusOK)
	update("blocked").Want(http.StatusUnprocessableEntity)
	if status() != "in_review" {
		t.Fatalf("refused write changed the status to %s", status())
	}
}
