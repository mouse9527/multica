// Added by the mouse9527/multica fork: merges the fork's route registrations
// (fork_private_project_routes_test.go) into upstream's
// TestEveryRouteByProjectObjectIDHidesPrivateProjects tables (M-166).

package main

func init() {
	for key, why := range forkRouteParamsOutsideProjects {
		routeParamsOutsideProjects[key] = outsideProjectParam{why: why}
	}
	for key, why := range forkPrivateProjectRoutesNotProbed {
		privateProjectRoutesNotProbed[key] = why
	}
}
