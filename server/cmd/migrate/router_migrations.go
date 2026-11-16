// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86).

package main

// routerConcurrentIndexCleanups lists the fork's Router migrations that build
// an index concurrently. Like githubIssueConcurrentIndexCleanups, they stay
// out of the upstream concurrentIndexCleanups literal so upstream syncs do not
// conflict on it; init merges them in.
var routerConcurrentIndexCleanups = map[string]string{
	"9005_router_subscription_scope_idx":    "idx_router_subscription_scope",
	"9006_router_event_key_idx":             "idx_router_event_key",
	"9007_router_event_subscription_idx":    "idx_router_event_subscription",
	"9009_router_dispatch_open_idx":         "idx_router_dispatch_open",
	"9010_router_dispatch_subscription_idx": "idx_router_dispatch_subscription",
	"9011_router_event_consumed_idx":        "idx_router_event_consumed",
	"9012_router_subscription_change_idx":   "idx_router_subscription_change",
	"9013_router_owner_notice_dedupe_idx":   "idx_owner_notice_dedupe",
}

func init() {
	for version, index := range routerConcurrentIndexCleanups {
		concurrentIndexCleanups[version] = index
		preMigrationHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
}
