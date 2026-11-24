// This file belongs to the mouse9527/multica fork (not present upstream): M-259.

package execenv

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	leanTestAgentID      = "0a1b2c3d-0000-4000-8000-000000000001"
	leanTestOtherAgentID = "0a1b2c3d-0000-4000-8000-000000000002"
)

func leanTestRepos(n int) []RepoContextForEnv {
	repos := make([]RepoContextForEnv, n)
	for i := range repos {
		repos[i] = RepoContextForEnv{URL: fmt.Sprintf("https://github.com/org/repo-%d", i+1)}
	}
	return repos
}

func leanTestRepoResource(url string) ProjectResourceForEnv {
	ref, _ := json.Marshal(map[string]string{"url": url})
	return ProjectResourceForEnv{ID: "res-1", ResourceType: "github_repo", ResourceRef: ref}
}

// leanTestIssueContext is a typical issue run in a workspace like this fork's:
// a project workflow, a project with one bound repo, workspace context and the
// platform skill.
func leanTestIssueContext() TaskContextForEnv {
	return TaskContextForEnv{
		IssueID:            "11111111-2222-3333-4444-555555555555",
		AgentID:            leanTestAgentID,
		AgentName:          "Dev",
		AgentInstructions:  "You are the developer agent.",
		WorkspaceContext:   "## Decisions\n\nThe decision section of the issue body wins over earlier plans.\n\n## Decision card\n\nAsk the owner with options A and B.",
		Repos:              []RepoContextForEnv{{URL: "https://github.com/org/app"}},
		ProjectID:          "proj-1",
		ProjectTitle:       "app",
		ProjectDescription: "Project rules live here.",
		ProjectResources:   []ProjectResourceForEnv{leanTestRepoResource("https://github.com/org/app")},
		AgentSkills:        []SkillContextForEnv{platformSkillFixture()},
		HasProjectWorkflow: true,
	}
}

// leanTestContexts covers every task kind and every branch that changes the
// set of headings.
func leanTestContexts() map[string]TaskContextForEnv {
	issue := leanTestIssueContext()

	builtinStatuses := issue
	builtinStatuses.HasProjectWorkflow = false
	builtinStatuses.IssueStatuses = []IssueStatusForEnv{{Key: "qa", Name: "QA", Category: "started"}}

	leader := issue
	leader.IsSquadLeader = true
	leader.HasProjectWorkflow = false

	requesting := issue
	requesting.RequestingUserName = "Owner"
	requesting.RequestingUserProfileDescription = "Prefers short answers."

	noProject := issue
	noProject.ProjectID, noProject.ProjectTitle, noProject.ProjectDescription, noProject.ProjectResources = "", "", "", nil
	noProject.Repos = leanTestRepos(15)

	pinned := issue
	pinned.Repos = []RepoContextForEnv{{URL: "https://github.com/org/app", Ref: "release/v2"}}

	return map[string]TaskContextForEnv{
		"issue-project-workflow": issue,
		"issue-builtin-statuses": builtinStatuses,
		"issue-squad-leader":     leader,
		"issue-requesting-user":  requesting,
		"issue-no-project":       noProject,
		"issue-pinned-repo":      pinned,
		"chat-web":               {ChatSessionID: "chat-1", AgentID: leanTestAgentID, AgentName: "Dev", Repos: leanTestRepos(2), AgentSkills: []SkillContextForEnv{platformSkillFixture()}},
		"chat-slack":             {ChatSessionID: "chat-1", ChatChannelType: ChannelTypeSlack, AgentID: leanTestAgentID, AgentName: "Dev"},
		"quick-create":           {QuickCreatePrompt: "make an issue", AgentID: leanTestAgentID, AgentName: "Dev", Repos: leanTestRepos(2), ProjectID: "proj-1", ProjectTitle: "app"},
		"autopilot":              {AutopilotRunID: "run-1", AutopilotID: "ap-1", AgentID: leanTestAgentID, AgentName: "Dev", Repos: leanTestRepos(2)},
	}
}

// briefHeadings returns the brief's markdown heading lines (levels 1–3) in
// order.
func briefHeadings(brief string) []string {
	var out []string
	for _, line := range strings.Split(brief, "\n") {
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			out = append(out, line)
		}
	}
	return out
}

func assertSameHeadings(t *testing.T, slim, lean string) {
	t.Helper()
	want, got := briefHeadings(slim), briefHeadings(lean)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("lean brief headings drifted from upstream; port the upstream change into runtime_config_lean.go.\nupstream:\n%s\n\nlean:\n%s",
			strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

// TestLeanBriefHeadingsMatchUpstream is the drift guard: an upstream sync that
// adds, renames, drops or reorders a section fails here until the lean brief
// follows.
func TestLeanBriefHeadingsMatchUpstream(t *testing.T) {
	t.Parallel()
	for name, ctx := range leanTestContexts() {
		name, ctx := name, ctx
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, provider := range []string{"claude", "codex", "traecli"} {
				assertSameHeadings(t, buildMetaSkillContentSlim(provider, ctx), buildMetaSkillContentLean(provider, ctx))
			}
		})
	}
}

// Not parallel: it switches the package-level runtimeGOOS.
func TestLeanBriefHeadingsMatchUpstreamOnWindows(t *testing.T) {
	saved := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = saved })
	runtimeGOOS = "windows"
	for name, ctx := range leanTestContexts() {
		slim, lean := buildMetaSkillContentSlim("claude", ctx), buildMetaSkillContentLean("claude", ctx)
		t.Run(name, func(t *testing.T) { assertSameHeadings(t, slim, lean) })
	}
	lean := buildMetaSkillContentLean("claude", leanTestIssueContext())
	for _, want := range []string{"PowerShell", "`--content-file <path>`", "`$OutputEncoding`", "`$LASTEXITCODE`", "never `/tmp` or shared paths"} {
		if !strings.Contains(lean, want) {
			t.Errorf("windows lean brief lacks %q", want)
		}
	}
}

// TestLeanBriefKeepsHardRules pins the rules the shorter text must not lose.
// Each entry names the rule so a failure says what went missing.
func TestLeanBriefKeepsHardRules(t *testing.T) {
	t.Parallel()
	brief := buildMetaSkillContentLean("claude", leanTestIssueContext())
	rules := []struct{ rule, phrase string }{
		{"turn exit is terminal", "Multica marks the task terminal the moment your top-level turn exits"},
		{"final comment is lost after exit", "the final comment you meant to post never sends"},
		{"no background-and-yield", "Never background-and-yield"},
		{"no standing by", "never end a turn \"standing by\""},
		{"no CI watch", "`gh pr checks --watch`"},
		{"no run watch", "`gh run watch`"},
		{"no sleep polls", "sleep/retry polls"},
		{"merge gate is not acceptance", "A repo's merge gate is NOT your acceptance criteria"},
		{"the one CI exception", "collect it as ONE foreground blocking call (`gh pr checks <pr> --watch`) inside this same turn"},
		{"never kill multica by name", "Never terminate `multica` or `multica.exe` by executable name"},
		{"daemon PID check", "`multica daemon status --output json`"},
		{"comment is the only delivery", "**Final results MUST be delivered via `multica issue comment add`.**"},
		{"logs are invisible", "The user does NOT see your terminal output or run logs"},
		{"final result only", "Post only your final result, before this turn exits"},
		{"file-first comments", "then post it with `--content-file <path>`"},
		{"no inline content", "Never use inline `--content`"},
		{"inline mangling incident", "MUL-2904"},
		{"heredoc incident", "#4182"},
		{"file inside workdir", "Write the file inside your working directory, never `/tmp` or shared paths"},
		{"comment add requires file", "Agent-authored bodies MUST use `--content-file`"},
		{"step 2 is mandatory", "Catch up on the comment history — this is mandatory"},
		{"step 2 scan command", "`--roots-only --summary --compact`"},
		{"step 2 expand command", "`--thread <id> --tail 30 --compact`"},
		{"step 2 has no silent waiver", "Silence about other threads is not a waiver"},
		{"step 4 is mandatory", "**Post your final results as a comment — this step is mandatory**"},
		{"step 4 parent of this turn", "`--parent` value given for THIS turn"},
		{"identity wins", "Agent Identity instructions have priority over the issue workflow below"},
		{"workflow status contract", "`[Project workflow]` in the per-turn message is this issue's status contract"},
		{"one issue one PR", "Default to one issue, one PR"},
		{"split by behavior", "slice by user-visible behavior, never by layer"},
		{"sub-issue skill pointer", "`references/sub-issues.md` in the `multica-platform` skill"},
		{"agent mention enqueues a run", "**enqueues a new run for that agent**"},
		{"member mention notifies", "**notifies a human**"},
		{"no local paths as links", "**Runtime-local paths are never deliverables.**"},
		{"stash rule", "never use `git stash` / `git stash pop` there"},
		{"CLI only", "never `curl` / `wget`"},
		{"no stdout merge", "never merge them (`2>&1`)"},
	}
	for _, r := range rules {
		if !strings.Contains(brief, r.phrase) {
			t.Errorf("lean brief lost rule %q: missing %q", r.rule, r.phrase)
		}
	}
}

// Workspace context carries hard rules such as the decision record and the
// decision card. The lean brief must pass it, the project description and the
// agent instructions through byte for byte, never summarised.
func TestLeanBriefPassesOwnerTextThrough(t *testing.T) {
	t.Parallel()
	ctx := leanTestIssueContext()
	brief := buildMetaSkillContentLean("claude", ctx)
	for _, text := range []string{"## Workspace Context\n\n" + ctx.WorkspaceContext + "\n\n", ctx.ProjectDescription, ctx.AgentInstructions} {
		if !strings.Contains(brief, text) {
			t.Errorf("lean brief does not carry %q verbatim", text)
		}
	}
}

func TestLeanBriefRepositories(t *testing.T) {
	t.Parallel()
	const kept = "KEPT an existing checkout"

	t.Run("workspace fallback is capped", func(t *testing.T) {
		ctx := leanTestContexts()["issue-no-project"]
		brief := buildMetaSkillContentLean("claude", ctx)
		for i := 1; i <= leanRepoListLimit; i++ {
			if !strings.Contains(brief, fmt.Sprintf("- https://github.com/org/repo-%d\n", i)) {
				t.Fatalf("repo %d missing:\n%s", i, brief)
			}
		}
		if strings.Contains(brief, "repo-6\n") || strings.Contains(brief, "repo-15\n") {
			t.Fatalf("repos past the cap are listed:\n%s", brief)
		}
		if !strings.Contains(brief, "- …and 10 more in this workspace; `multica repo list` shows them all.") {
			t.Fatalf("missing the overflow line:\n%s", brief)
		}
		if !strings.Contains(buildMetaSkillContentSlim("claude", ctx), "repo-15\n") {
			t.Fatal("the upstream brief must keep listing every repo")
		}
	})

	t.Run("project with no repo resource falls back and is capped", func(t *testing.T) {
		ctx := leanTestIssueContext()
		ctx.ProjectResources = nil
		ctx.Repos = leanTestRepos(7)
		brief := buildMetaSkillContentLean("claude", ctx)
		if !strings.Contains(brief, "- …and 2 more in this workspace") || strings.Contains(brief, "repo-6\n") {
			t.Fatalf("project without repo resources should be capped:\n%s", brief)
		}
	})

	t.Run("project repos are never capped", func(t *testing.T) {
		ctx := leanTestIssueContext()
		ctx.Repos = leanTestRepos(7)
		brief := buildMetaSkillContentLean("claude", ctx)
		if !strings.Contains(brief, "repo-7\n") || strings.Contains(brief, "more in this workspace") {
			t.Fatalf("project repos must all be listed:\n%s", brief)
		}
	})

	t.Run("exactly at the cap has no overflow line", func(t *testing.T) {
		ctx := leanTestContexts()["issue-no-project"]
		ctx.Repos = leanTestRepos(leanRepoListLimit)
		brief := buildMetaSkillContentLean("claude", ctx)
		if strings.Contains(brief, "more in this workspace") {
			t.Fatalf("no overflow expected at the cap:\n%s", brief)
		}
	})

	t.Run("kept-checkout paragraph moves to the CLI output", func(t *testing.T) {
		ctx := leanTestIssueContext()
		if strings.Contains(buildMetaSkillContentLean("claude", ctx), kept) {
			t.Fatal("lean brief should not carry the kept-checkout paragraph")
		}
		if !strings.Contains(buildMetaSkillContentSlim("claude", ctx), kept) {
			t.Fatal("upstream brief changed: the kept-checkout anchor is gone, recheck the CLI note")
		}
	})

	t.Run("pinned repo keeps the base-branch rule", func(t *testing.T) {
		brief := buildMetaSkillContentLean("claude", leanTestContexts()["issue-pinned-repo"])
		for _, want := range []string{"(starts from `release/v2`)", "`gh pr create --base <that-branch>`", "confirm the target branch before opening a pull request"} {
			if !strings.Contains(brief, want) {
				t.Fatalf("pinned repo lacks %q:\n%s", want, brief)
			}
		}
	})
}

func TestLeanBriefListed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		list, agent string
		want        bool
	}{
		{"", leanTestAgentID, false},
		{leanTestAgentID, "", false},
		{",,", "", false},
		{leanTestAgentID, leanTestAgentID, true},
		{" " + leanTestOtherAgentID + " , " + strings.ToUpper(leanTestAgentID) + " ", leanTestAgentID, true},
		{leanTestOtherAgentID, leanTestAgentID, false},
		{leanTestAgentID[:8], leanTestAgentID, false},
	} {
		if got := leanBriefListed(tc.list, tc.agent); got != tc.want {
			t.Errorf("leanBriefListed(%q, %q) = %v, want %v", tc.list, tc.agent, got, tc.want)
		}
	}
}

// Not parallel: t.Setenv changes the process environment.
func TestBuildMetaSkillContentLeanSwitch(t *testing.T) {
	ctx := leanTestIssueContext()
	slim := buildMetaSkillContentSlim("claude", ctx)
	lean := buildMetaSkillContentLean("claude", ctx)
	if slim == lean {
		t.Fatal("fixture renders the same brief both ways")
	}

	t.Setenv(leanBriefAgentsEnv, "")
	if got := buildMetaSkillContent("claude", ctx); got != slim {
		t.Fatal("empty list must render the upstream brief byte for byte")
	}

	t.Setenv(leanBriefAgentsEnv, leanTestOtherAgentID)
	if got := buildMetaSkillContent("claude", ctx); got != slim {
		t.Fatal("an unlisted agent must render the upstream brief")
	}

	t.Setenv(leanBriefAgentsEnv, leanTestOtherAgentID+","+leanTestAgentID)
	if got := buildMetaSkillContent("claude", ctx); got != lean {
		t.Fatal("a listed agent must render the lean brief")
	}

	// One daemon serves several workspaces. The list matches agent IDs only,
	// so an agent of another workspace with a different ID — even with a large
	// repo list and no project — keeps the upstream brief.
	other := leanTestContexts()["issue-no-project"]
	other.AgentID = "0a1b2c3d-0000-4000-8000-0000000000ff"
	other.WorkspaceContext = "Another workspace's rules."
	if got := buildMetaSkillContent("claude", other); got != buildMetaSkillContentSlim("claude", other) {
		t.Fatal("an agent of another workspace that is not listed must keep the upstream brief")
	}
}

// TestLeanBriefByteComparison reports lean against upstream for the three
// task kinds the rollout compares (run with -v to see the numbers), and fails
// if a kind stops saving a meaningful amount. The floors sit 15–20% under
// the first CI measurement (issue 6191, chat 3106, quick-create 1631 bytes).
func TestLeanBriefByteComparison(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ctx      TaskContextForEnv
		minSaved int
	}{
		{"issue", leanTestIssueContext(), 5000},
		{"chat", leanTestContexts()["chat-web"], 2500},
		{"quick-create", leanTestContexts()["quick-create"], 1300},
	}
	var report strings.Builder
	for _, tc := range cases {
		slim := len(buildMetaSkillContentSlim("claude", tc.ctx))
		lean := len(buildMetaSkillContentLean("claude", tc.ctx))
		fmt.Fprintf(&report, "%s: upstream %d bytes, lean %d bytes, saved %d (%.1f%%)\n", tc.name, slim, lean, slim-lean, 100*float64(slim-lean)/float64(slim))
		if slim-lean < tc.minSaved {
			t.Errorf("%s: lean saves %d bytes, want at least %d", tc.name, slim-lean, tc.minSaved)
		}
	}
	t.Log("\n" + report.String())
}
