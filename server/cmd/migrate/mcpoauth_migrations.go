// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package main

// mcpOAuthConcurrentIndexCleanups lists the fork's MCP OAuth migrations that
// build an index concurrently. Like routerConcurrentIndexCleanups, they stay
// out of the upstream concurrentIndexCleanups literal so upstream syncs do not
// conflict on it; init merges them in.
var mcpOAuthConcurrentIndexCleanups = map[string]string{
	"9024_mcp_oauth_server_agent_name_idx": "idx_mcp_oauth_server_agent_name",
}

// mcpOAuthConcurrentDownIndexCleanups lists the fork's MCP OAuth migrations
// whose down direction rebuilds an index concurrently, for the same reason
// concurrentDownIndexCleanups exists.
var mcpOAuthConcurrentDownIndexCleanups = map[string]string{
	"9025_drop_mcp_oauth_server_agent_name_idx": "idx_mcp_oauth_server_agent_name",
}

func init() {
	for version, index := range mcpOAuthConcurrentIndexCleanups {
		concurrentIndexCleanups[version] = index
		preMigrationHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
	for version, index := range mcpOAuthConcurrentDownIndexCleanups {
		concurrentDownIndexCleanups[version] = index
		preRollbackHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
}
