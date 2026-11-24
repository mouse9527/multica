-- Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
-- One OAuth server per name per agent: the name is its key in the agent's
-- mcpServers. Also the lookup the claim path uses (servers of an agent).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_mcp_oauth_server_agent_name
    ON mcp_oauth_server (agent_id, name);
