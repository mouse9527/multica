-- Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
--
-- Three new tables; no existing table is altered. Every table carries
-- workspace_id so workspace teardown needs no join. No foreign keys (repo
-- rule): mcpoauth.DeleteWorkspaceData removes the rows with the workspace, and
-- deleting a server removes its connections and pending authorizations.
--
-- The DDL runs through EXECUTE inside a DO block so sqlc, which reads this
-- directory as its schema, does not see these tables (the Router convention,
-- migration 9004): the mcpoauth package uses hand-written SQL only, so the
-- shared pkg/db/generated/models.go stays as upstream has it.
--
-- Every *_ciphertext column is secretbox (AES-256-GCM) output. Tokens, client
-- secrets, PKCE verifiers and extra header values never appear in plaintext.

DO $mcp_oauth$
BEGIN
    -- An OAuth-protected MCP server configured on one agent. name is the key
    -- the server gets in the agent's mcpServers at claim time. Extra headers
    -- (for example a gateway's own API key header) are sealed as one JSON
    -- object and are never returned by the API.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS mcp_oauth_server (
        id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id             UUID NOT NULL,
        agent_id                 UUID NOT NULL,
        name                     TEXT NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$'),
        url                      TEXT NOT NULL CHECK (char_length(url) BETWEEN 9 AND 2048 AND url LIKE 'https://%'),
        scope                    TEXT NOT NULL DEFAULT '' CHECK (char_length(scope) <= 1024),
        extra_headers_ciphertext BYTEA CHECK (extra_headers_ciphertext IS NULL OR octet_length(extra_headers_ciphertext) > 0),
        created_by               UUID NOT NULL,
        created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;

    -- One user's grant for one server. P1 connects as the agent's owner; the
    -- (server_id, user_id) key leaves room for other identities later. A
    -- needs_reconnect row keeps no token: the refresh token was rejected or
    -- lost, so only a new connect can bring it back.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS mcp_oauth_connection (
        server_id                  UUID NOT NULL,
        user_id                    UUID NOT NULL,
        workspace_id               UUID NOT NULL,
        status                     TEXT NOT NULL CHECK (status IN ('connected', 'needs_reconnect')),
        resource                   TEXT NOT NULL CHECK (char_length(resource) BETWEEN 9 AND 2048),
        token_endpoint             TEXT NOT NULL CHECK (char_length(token_endpoint) BETWEEN 9 AND 2048),
        client_id                  TEXT NOT NULL CHECK (char_length(client_id) BETWEEN 1 AND 2048),
        client_secret_ciphertext   BYTEA,
        token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none'
            CHECK (token_endpoint_auth_method IN ('none', 'client_secret_post', 'client_secret_basic')),
        access_token_ciphertext    BYTEA,
        refresh_token_ciphertext   BYTEA,
        expires_at                 TIMESTAMPTZ,
        granted_scope              TEXT NOT NULL DEFAULT '' CHECK (char_length(granted_scope) <= 4096),
        last_error                 TEXT NOT NULL DEFAULT '' CHECK (char_length(last_error) <= 64),
        connected_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
        refreshed_at               TIMESTAMPTZ,
        updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
        PRIMARY KEY (server_id, user_id),
        CHECK (status <> 'connected' OR access_token_ciphertext IS NOT NULL)
    )
    $ddl$;

    -- A started, not yet completed authorization. state_hash is SHA-256 of
    -- the state sent to the authorization server; the callback deletes the
    -- row as it reads it, so a state works once. The client registration made
    -- for this attempt travels here until the grant is saved.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS mcp_oauth_state (
        state_hash                 BYTEA PRIMARY KEY CHECK (octet_length(state_hash) = 32),
        workspace_id               UUID NOT NULL,
        server_id                  UUID NOT NULL,
        user_id                    UUID NOT NULL,
        resource                   TEXT NOT NULL CHECK (char_length(resource) BETWEEN 9 AND 2048),
        token_endpoint             TEXT NOT NULL CHECK (char_length(token_endpoint) BETWEEN 9 AND 2048),
        client_id                  TEXT NOT NULL CHECK (char_length(client_id) BETWEEN 1 AND 2048),
        client_secret_ciphertext   BYTEA,
        token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none'
            CHECK (token_endpoint_auth_method IN ('none', 'client_secret_post', 'client_secret_basic')),
        redirect_uri               TEXT NOT NULL CHECK (char_length(redirect_uri) BETWEEN 9 AND 2048),
        verifier_ciphertext        BYTEA NOT NULL CHECK (octet_length(verifier_ciphertext) > 0),
        return_to                  TEXT NOT NULL DEFAULT '' CHECK (char_length(return_to) <= 2048),
        expires_at                 TIMESTAMPTZ NOT NULL,
        created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
        CHECK (expires_at > created_at)
    )
    $ddl$;
END
$mcp_oauth$;
