-- Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
-- Rebuilds the 9024 index on the mcp_oauth_server table 9026's down recreates.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_mcp_oauth_server_agent_name
    ON mcp_oauth_server (agent_id, name);
