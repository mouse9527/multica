// Added by the mouse9527/multica fork: choosing the repository of a new
// GitHub-native issue. Upstream has no such file.

package handler

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// createRepoAgentActor returns an agent and a running task of it, so a
// request carrying both resolves to that agent.
func createRepoAgentActor(t *testing.T) (agentID, taskID string) {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent").Scan(&agentID, &runtimeID)
	taskID = dbfx.Insert(t, "agent_task_queue", testutil.Cols{
		"agent_id": agentID, "runtime_id": runtimeID, "status": "running", "priority": 0,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	return agentID, taskID
}

func githubRepoResource(t *testing.T, projectID, repo string) string {
	t.Helper()
	return dbfx.Insert(t, "project_resource", testutil.Cols{
		"project_id": projectID, "workspace_id": testWorkspaceID,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/` + repo + `"}`,
	})
}

// The repository of a new issue: named, the project's default, the
// project's only one, or asked for; agents always name it or ask for a local
// issue.
func TestCreateIssueChoosesGitHubRepository(t *testing.T) {
	h, fake, _ := newGitHubWriteHandler(t)
	agentID, taskID := createRepoAgentActor(t)

	none := dbfx.Project(t, "No repositories")
	single := dbfx.Project(t, "One repository")
	githubRepoResource(t, single, "acme/widgets")
	multi := dbfx.Project(t, "Two repositories")
	widgets := githubRepoResource(t, multi, "acme/widgets")
	githubRepoResource(t, multi, "acme/gadgets")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = ANY($1::uuid[])`, []string{none, single, multi})
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project_github_settings WHERE workspace_id = $1`, testWorkspaceID)
	})

	create := func(h *Handler, agent bool, body map[string]any) *testutil.Response {
		// backlog is the entry status, which GitHub shows without a mapping.
		body["status"] = "backlog"
		req := newRequest(http.MethodPost, "/api/issues", body)
		if agent {
			req.Header.Set("X-Agent-ID", agentID)
			req.Header.Set("X-Task-ID", taskID)
		}
		return testutil.Call(t, h.CreateIssue, req)
	}
	wantLocal := func(t *testing.T, resp *testutil.Response) {
		t.Helper()
		issue := resp.Want(http.StatusCreated).Map()
		if issue["provider"] != "local" {
			t.Fatalf("issue = %v, want local", issue)
		}
		if requests, _ := fake.take(); len(requests) != 0 {
			t.Fatalf("local create reached GitHub: %v", requests)
		}
	}
	wantGitHub := func(t *testing.T, resp *testutil.Response) {
		t.Helper()
		issue := resp.Want(http.StatusCreated).Map()
		if issue["provider"] != "github" {
			t.Fatalf("issue = %v, want github", issue)
		}
		if requests, _ := fake.take(); len(requests) != 1 || requests[0] != "POST /repositories/4242/issues" {
			t.Fatalf("GitHub requests = %v", requests)
		}
	}
	wantRefused := func(t *testing.T, resp *testutil.Response, status int, code string, candidates ...string) map[string]any {
		t.Helper()
		body := resp.Want(status).Map()
		if body["code"] != code {
			t.Fatalf("body = %v, want code %s", body, code)
		}
		var got []string
		for _, c := range body["candidates"].([]any) {
			got = append(got, c.(string))
		}
		if !slices.Equal(got, candidates) {
			t.Fatalf("candidates = %v, want %v", got, candidates)
		}
		if requests, _ := fake.take(); len(requests) != 0 {
			t.Fatalf("refused create reached GitHub: %v", requests)
		}
		return body
	}
	issuesIn := func(projectID string) int {
		return dbfx.Count(t, `SELECT count(*) FROM issue WHERE project_id = $1`, projectID)
	}

	t.Run("project without repositories stays local", func(t *testing.T) {
		wantLocal(t, create(h, false, map[string]any{"title": "Plain", "project_id": none}))
		wantLocal(t, create(h, true, map[string]any{"title": "Plain by agent", "project_id": none}))
		wantLocal(t, create(h, true, map[string]any{"title": "No project"}))
	})

	t.Run("single repository is chosen for a member", func(t *testing.T) {
		wantGitHub(t, create(h, false, map[string]any{"title": "Auto", "project_id": single}))
		wantLocal(t, create(h, false, map[string]any{"title": "Kept local", "project_id": single, "provider": "local"}))
	})

	t.Run("agent must name the repository or provider local", func(t *testing.T) {
		before := issuesIn(single)
		wantRefused(t, create(h, true, map[string]any{"title": "Guessing", "project_id": single}),
			http.StatusUnprocessableEntity, "github_repo_required", "acme/widgets")
		if issuesIn(single) != before {
			t.Fatal("refused create wrote an issue")
		}
		wantLocal(t, create(h, true, map[string]any{"title": "Agent local", "project_id": single, "provider": "local"}))
		wantGitHub(t, create(h, true, map[string]any{"title": "Agent named", "project_id": single, "repo": "acme/widgets"}))
	})

	t.Run("sub-issue takes its parent's project", func(t *testing.T) {
		parent := dbfx.Issue(t, "Parent", testutil.Cols{"project_id": single})
		wantRefused(t, create(h, true, map[string]any{"title": "Child", "parent_issue_id": parent}),
			http.StatusUnprocessableEntity, "github_repo_required", "acme/widgets")
	})

	t.Run("several repositories without a default are ambiguous", func(t *testing.T) {
		wantRefused(t, create(h, false, map[string]any{"title": "Which", "project_id": multi}),
			http.StatusUnprocessableEntity, "github_repo_ambiguous", "acme/widgets", "acme/gadgets")
		wantRefused(t, create(h, true, map[string]any{"title": "Which by agent", "project_id": multi}),
			http.StatusUnprocessableEntity, "github_repo_ambiguous", "acme/widgets", "acme/gadgets")
	})

	t.Run("project default", func(t *testing.T) {
		settings := testutil.Call(t, h.SetProjectGitHubSettings, withURLParam(newRequest(http.MethodPut,
			"/api/projects/"+multi+"/github-settings", map[string]any{"default_resource_id": widgets}), "id", multi)).
			Want(http.StatusOK).Map()
		if settings["default_resource_id"] != widgets || len(settings["repos"].([]any)) != 2 {
			t.Fatalf("settings = %v", settings)
		}
		got := testutil.Call(t, h.GetProjectGitHubSettings, withURLParam(newRequest(http.MethodGet,
			"/api/projects/"+multi+"/github-settings", nil), "id", multi)).Want(http.StatusOK).Map()
		if got["default_resource_id"] != widgets {
			t.Fatalf("read settings = %v", got)
		}
		wantGitHub(t, create(h, false, map[string]any{"title": "Default", "project_id": multi}))
		body := wantRefused(t, create(h, true, map[string]any{"title": "Default by agent", "project_id": multi}),
			http.StatusUnprocessableEntity, "github_repo_required", "acme/widgets", "acme/gadgets")
		if body["default_repo"] != "acme/widgets" {
			t.Fatalf("refusal = %v, want default_repo", body)
		}

		// Only a repository of the project can be its default.
		other := githubRepoResource(t, single, "acme/other")
		testutil.Call(t, h.SetProjectGitHubSettings, withURLParam(newRequest(http.MethodPut,
			"/api/projects/"+multi+"/github-settings", map[string]any{"default_resource_id": other}), "id", multi)).
			Want(http.StatusBadRequest)
		cleared := testutil.Call(t, h.SetProjectGitHubSettings, withURLParam(newRequest(http.MethodPut,
			"/api/projects/"+multi+"/github-settings", map[string]any{"default_resource_id": nil}), "id", multi)).
			Want(http.StatusOK).Map()
		if cleared["default_resource_id"] != nil {
			t.Fatalf("cleared settings = %v", cleared)
		}
	})

	t.Run("malformed choices", func(t *testing.T) {
		wantRefused(t, create(h, false, map[string]any{"title": "Bad", "project_id": single, "provider": "gitlab"}),
			http.StatusBadRequest, "github_provider_invalid")
		wantRefused(t, create(h, false, map[string]any{"title": "Both", "project_id": single, "provider": "local", "repo": "acme/widgets"}),
			http.StatusBadRequest, "github_provider_invalid")
		wantRefused(t, create(h, true, map[string]any{"title": "Unknown", "project_id": single, "repo": "acme/unknown"}),
			http.StatusUnprocessableEntity, "github_repo_not_registered", "acme/widgets")
		wantRefused(t, create(h, false, map[string]any{"title": "GitHub nowhere", "project_id": none, "provider": "github"}),
			http.StatusUnprocessableEntity, "github_repo_required")
	})

	t.Run("feature off ignores both fields", func(t *testing.T) {
		off := *h
		off.GitHubIssues = nil
		wantLocal(t, create(&off, true, map[string]any{"title": "Off", "project_id": single}))
		wantLocal(t, create(&off, false, map[string]any{"title": "Off named", "project_id": single, "repo": "acme/widgets", "provider": "gitlab"}))
	})
}
