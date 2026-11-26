-- Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).
--
-- The sealed client secrets go with the table. Entries that name a
-- pre-registered client keep their client_id in their config; a server rolled
-- back past this migration no longer accepts those fields, so remove them (or
-- the entries) before running an older image.
DROP TABLE IF EXISTS mcp_oauth_client;
