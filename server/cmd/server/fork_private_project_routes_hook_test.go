// Added by the mouse9527/multica fork: merges the fork's route registrations
// (fork_private_project_routes_test.go) into upstream's
// TestEveryRouteByProjectObjectIDHidesPrivateProjects tables (M-166, M-277).

package main

func init() {
	for key, why := range forkRouteParamsOutsideProjects {
		// Upstream's own entry wins: it may carry a probe value.
		if _, ok := routeParamsOutsideProjects[key]; !ok {
			routeParamsOutsideProjects[key] = outsideProjectParam{why: why}
		}
	}
	for key := range forkIssueRouteParams {
		projectScopedRouteParams[key] = func(f *privateRouteFixture, _ string) string { return f.issue() }
	}
	for key, why := range forkPrivateProjectRoutesNotProbed {
		privateProjectRoutesNotProbed[key] = why
	}
}
