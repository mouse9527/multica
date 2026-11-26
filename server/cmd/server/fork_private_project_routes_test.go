// Added by the mouse9527/multica fork: how the fork's routes register with
// upstream's TestEveryRouteByProjectObjectIDHidesPrivateProjects (M-166).
// Modified for the mouse9527/multica fork: issue-naming parameters (M-277).
// Modified for the mouse9527/multica fork: cross-workspace read grants (M-307).
//
// Upstream's test (private_project_routes_test.go) refuses any route
// parameter it has no entry for, even on a route it does not probe. The
// fork's entries live here instead of in upstream's tables, and
// fork_private_project_routes_hook_test.go merges them in (see
// docs/fork-upstream-sync.md). TestForkPrivateProjectRouteRegistrations keeps
// them in step with the fork's routes.

package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

// forkRoutePrefixes are the route trees only the fork registers.
var forkRoutePrefixes = []string{"/api/router/", "/api/evolution/", "/api/agents/{id}/mcp-oauth/", "/api/workspaces/{id}/mcp-oauth/", "/api/mcp-oauth/", "/api/workspaces/{id}/cross-read-grants/"}

// forkRouteParamsOutsideProjects are fork route parameters that cannot name
// an object inside a project, keyed as upstream keys them ("subscriptions/{id}"),
// each with the reason. They go into upstream's routeParamsOutsideProjects.
var forkRouteParamsOutsideProjects = map[string]string{
	"subscriptions/{id}": "a Super Agent subscription, a workspace object like an agent: its scope can span projects, and the issues and events it reaches drop out for a caller outside their private project instead (TestRouterSubscriptionFiltersPrivateProjects)",
	// M-230, M-269: OAuth connections to MCP entries of an agent or of the
	// workspace MCP library.
	"agents/{id}":          "an agent, a workspace object: agents are not inside projects",
	"workspaces/{id}":      "the workspace itself, which contains the projects",
	"mcp-oauth/{name}":     "the name of an MCP entry in an agent's own mcp_config; it belongs to the agent, not to a project",
	"mcp-oauth/{serverId}": "a workspace MCP library entry; it belongs to the workspace, not to a project",
	// M-307: cross-workspace read grants a workspace gives.
	"cross-read-grants/{grantId}": "a cross-workspace read grant, a workspace object: it covers the whole workspace, not a project",
}

// forkIssueRouteParams are fork route parameters that name an issue under a
// key upstream does not know, each with the reason. They go into upstream's
// projectScopedRouteParams, resolving to the probe fixture's issue; every
// route declaring one must be probed or listed in
// forkPrivateProjectRoutesNotProbed.
var forkIssueRouteParams = map[string]string{
	"proposals/{issueId}": "an Evolution proposal is its issue, loaded through loadIssueForUser (M-138)",
}

// forkPrivateProjectRoutesNotProbed are fork routes declaring a
// project-scoped parameter that upstream's probe cannot judge,
// each with the reason. They go into upstream's privateProjectRoutesNotProbed.
var forkPrivateProjectRoutesNotProbed = map[string]string{
	"GET /api/router/issues/{id}/covering": "404 unless router_v1 is on, and the probe's router has no feature flags; TestRouterIssueCoveringHidesPrivateProjects probes it with router_v1 on",
	// M-138: Evolution proposals, loaded through loadIssueForUser.
	"GET /api/evolution/proposals/{issueId}":           "404 unless evolution_v1 is on, and the probe's router has no feature flags; TestEvolutionProposalInPrivateProject probes it with evolution_v1 on",
	"POST /api/evolution/proposals/{issueId}/apply":    "404 unless evolution_v1 is on, and the probe's router has no feature flags; non-admins get 403 before the issue is loaded, and owners and admins load it through loadIssueForUser, 404 outside the private project (TestEvolutionProposalInPrivateProject)",
	"POST /api/evolution/proposals/{issueId}/rollback": "404 unless evolution_v1 is on, and the probe's router has no feature flags; non-admins get 403 before the issue is loaded, and owners and admins load it through loadIssueForUser, 404 outside the private project (TestEvolutionProposalInPrivateProject)",
}

var forkChiRouteParam = regexp.MustCompile(`\{([^}:]+)(?::[^}]*)?\}`)

// forkRouteParamKeys returns the parameters a chi pattern declares, each keyed
// by the last literal segment before it, as upstream's test keys them.
func forkRouteParamKeys(pattern string) []string {
	var keys []string
	literal := ""
	for _, segment := range strings.Split(strings.Trim(pattern, "/"), "/") {
		m := forkChiRouteParam.FindStringSubmatch(segment)
		if m == nil || m[0] != segment {
			literal = segment
			continue
		}
		keys = append(keys, literal+"/{"+m[1]+"}")
	}
	return keys
}

// Every parameter a fork route declares is registered here or upstream knows
// it, as upstream's test requires even of a route it does not probe; a route
// with a project-scoped parameter is listed as not probed (none is probed by
// upstream's test today); and every registration is still in use.
func TestForkPrivateProjectRouteRegistrations(t *testing.T) {
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	usedParams := map[string]bool{}
	walked := map[string]bool{}
	err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + pattern
		walked[key] = true
		fork := false
		for _, prefix := range forkRoutePrefixes {
			fork = fork || strings.HasPrefix(pattern, prefix)
		}
		if !fork {
			return nil
		}
		_, notProbed := forkPrivateProjectRoutesNotProbed[key]
		scoped := false
		for _, p := range forkRouteParamKeys(pattern) {
			if _, ok := forkRouteParamsOutsideProjects[p]; ok {
				usedParams[p] = true
				continue
			}
			if _, ok := forkIssueRouteParams[p]; ok {
				usedParams[p] = true
				scoped = true
				continue
			}
			if _, ok := projectScopedRouteParams[p]; ok {
				scoped = true
				continue
			}
			if _, ok := routeParamsOutsideProjects[p]; ok {
				continue
			}
			t.Errorf("%s declares %s: add it to forkRouteParamsOutsideProjects with the reason it cannot name an object inside a project, or to forkIssueRouteParams if it names an issue", key, p)
		}
		if scoped && !notProbed {
			t.Errorf("%s declares a project-scoped parameter: list it in forkPrivateProjectRoutesNotProbed with the test that probes it", key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	for key := range forkIssueRouteParams {
		if !usedParams[key] {
			t.Errorf("forkIssueRouteParams lists %s, which no fork route declares", key)
		}
	}
	for key := range forkRouteParamsOutsideProjects {
		if !usedParams[key] {
			t.Errorf("forkRouteParamsOutsideProjects lists %s, which no fork route declares", key)
		}
	}
	for key := range forkPrivateProjectRoutesNotProbed {
		if !walked[key] {
			t.Errorf("forkPrivateProjectRoutesNotProbed lists %q, which the router no longer registers", key)
		}
	}
}
