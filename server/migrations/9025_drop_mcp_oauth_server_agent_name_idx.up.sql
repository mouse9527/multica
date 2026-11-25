-- Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
-- mcp_oauth_server goes away in 9026; its index goes first, on its own, as
-- every concurrent index change does.
DROP INDEX CONCURRENTLY IF EXISTS idx_mcp_oauth_server_agent_name;
