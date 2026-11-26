// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package main

// crossReadConcurrentIndexCleanups lists the fork's cross-workspace read
// migrations that build an index concurrently. Like
// mcpOAuthConcurrentIndexCleanups, they stay out of the upstream
// concurrentIndexCleanups literal so upstream syncs do not conflict on it;
// init merges them in.
var crossReadConcurrentIndexCleanups = map[string]string{
	"9029_cross_workspace_read_grant_lookup_idx": "idx_cross_workspace_read_grant_lookup",
	"9030_cross_workspace_read_audit_target_idx": "idx_cross_workspace_read_audit_target",
}

func init() {
	for version, index := range crossReadConcurrentIndexCleanups {
		concurrentIndexCleanups[version] = index
		preMigrationHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
}
