// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package main

// mcpOAuthConcurrentIndexCleanups lists the fork's MCP OAuth migrations that
// build an index concurrently. Like routerConcurrentIndexCleanups, they stay
// out of the upstream concurrentIndexCleanups literal so upstream syncs do not
// conflict on it; init merges them in.
var mcpOAuthConcurrentIndexCleanups = map[string]string{
	"9024_mcp_oauth_server_agent_name_idx": "idx_mcp_oauth_server_agent_name",
}

func init() {
	for version, index := range mcpOAuthConcurrentIndexCleanups {
		concurrentIndexCleanups[version] = index
		preMigrationHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
}
