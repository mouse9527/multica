-- Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).
--
-- One new table; no existing table is altered. An OAuth MCP entry may name a
-- client registered in advance with its authorization server
-- ("auth": {"client_id": ..., "issuer": ...}). The entry is a plain JSON
-- config that privileged users can read, export and copy, so the client's
-- secret is never part of it: it is written through its own endpoint, sealed
-- with secretbox (AES-256-GCM), and never returned.
--
-- server_ref is the entry's ref, as in mcp_oauth_connection (9026). No
-- foreign keys (repo rule): the secret is deleted in application code with
-- its entry (mcpoauth.DeleteEntryData, DeleteAgentEntryDataExcept) and its
-- workspace (mcpoauth.DeleteWorkspaceData). As in 9023, the DDL runs through
-- EXECUTE inside a DO block so sqlc does not see the table.

DO $mcp_oauth$
BEGIN
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS mcp_oauth_client (
        server_ref               TEXT PRIMARY KEY
            CHECK (server_ref ~ '^(ws:[0-9a-f-]{36}|agent:[0-9a-f-]{36}:[A-Za-z0-9_-]{1,64})$'),
        workspace_id             UUID NOT NULL,
        client_secret_ciphertext BYTEA NOT NULL CHECK (octet_length(client_secret_ciphertext) > 0),
        updated_by               UUID NOT NULL,
        updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;
END
$mcp_oauth$;
