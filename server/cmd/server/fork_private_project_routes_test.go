// Added by the mouse9527/multica fork: how the fork's routes register with
// upstream's TestEveryRouteByProjectObjectIDHidesPrivateProjects (M-166).
//
// Upstream's test (private_project_routes_test.go, not yet merged into the
// fork) refuses any route parameter it has no entry for. The fork's entries
// live here instead of in upstream's tables; after the upstream sync that
// brings the test in, fork_private_project_routes_hook_test.go merges them
// in (see docs/fork-upstream-sync.md). Until then
// TestForkPrivateProjectRouteRegistrations keeps them in step with the router.

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
var forkRoutePrefixes = []string{"/api/router/", "/api/evolution/"}

// forkRouteParamsOutsideProjects are fork route parameters that cannot name
// an object inside a project, keyed as upstream keys them ("subscriptions/{id}"),
// each with the reason. They go into upstream's routeParamsOutsideProjects.
var forkRouteParamsOutsideProjects = map[string]string{
	"subscriptions/{id}": "a Super Agent subscription, a workspace object like an agent: its scope can span projects, and the issues and events it reaches drop out for a caller outside their private project instead (TestRouterSubscriptionFiltersPrivateProjects)",
}

// forkPrivateProjectRoutesNotProbed are fork routes declaring one of
// upstream's project-scoped parameters that upstream's probe cannot judge,
// each with the reason. They go into upstream's privateProjectRoutesNotProbed.
var forkPrivateProjectRoutesNotProbed = map[string]string{
	"GET /api/router/issues/{id}/covering": "404 unless router_v1 is on, and the probe's router has no feature flags; TestRouterIssueCoveringHidesPrivateProjects probes it with router_v1 on",
	// M-138: Evolution proposals, loaded through loadIssueForUser.
	"GET /api/evolution/proposals/{issueId}":           "404 unless evolution_v1 is on, and the probe's router has no feature flags; TestEvolutionProposalInPrivateProject probes it with evolution_v1 on",
	"POST /api/evolution/proposals/{issueId}/apply":    "404 unless evolution_v1 is on; owners and admins only, who see every private project, and everyone else gets 403 before the issue is loaded (TestEvolutionProposalInPrivateProject)",
	"POST /api/evolution/proposals/{issueId}/rollback": "404 unless evolution_v1 is on; owners and admins only, who see every private project, and everyone else gets 403 before the issue is loaded (TestEvolutionProposalInPrivateProject)",
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

// Every parameter a fork route declares is registered here or the route is
// listed as not probed, and every registration is still in use, so the
// upstream sync needs nothing beyond the hook.
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
		for _, p := range forkRouteParamKeys(pattern) {
			if _, ok := forkRouteParamsOutsideProjects[p]; ok {
				usedParams[p] = true
				continue
			}
			if !notProbed {
				t.Errorf("%s declares %s: add it to forkRouteParamsOutsideProjects with the reason it cannot name an object inside a project, or list the route in forkPrivateProjectRoutesNotProbed with the test that probes it", key, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
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
