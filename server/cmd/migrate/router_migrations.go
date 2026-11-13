// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package main

// routerConcurrentIndexCleanups lists the fork's Router migrations that build
// an index concurrently. Like githubIssueConcurrentIndexCleanups, they stay
// out of the upstream concurrentIndexCleanups literal so upstream syncs do not
// conflict on it; init merges them in.
var routerConcurrentIndexCleanups = map[string]string{
	"9005_router_subscription_scope_idx": "idx_router_subscription_scope",
	"9006_router_event_key_idx":          "idx_router_event_key",
	"9007_router_event_subscription_idx": "idx_router_event_subscription",
}

func init() {
	for version, index := range routerConcurrentIndexCleanups {
		concurrentIndexCleanups[version] = index
		preMigrationHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
}
