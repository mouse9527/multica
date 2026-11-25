-- Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
--
-- OAuth stops being a separate kind of MCP server: an entry in an agent's
-- mcp_config or in the workspace MCP library declares "auth": {"type":
-- "oauth"}, so mcp_oauth_server is dropped. Connections and pending
-- authorizations are keyed by the entry's ref instead of a server id:
--   ws:<workspace_mcp_server.id>    a workspace library entry
--   agent:<agent id>:<entry name>   an entry of one agent's own mcp_config
-- and remember the entry URL the grant was made for (server_url), so a token
-- is never sent to a URL the entry was edited to later.
--
-- Only this fork's own tables are touched. The old tables are replaced, not
-- converted: a connection to an mcp_oauth_server row has no entry to point
-- at. So that no grant is dropped unnoticed, the migration refuses to run
-- while mcp_oauth_server or mcp_oauth_connection holds a row; those rows
-- have to be removed (disconnected) first. Pending authorizations are
-- short-lived and are discarded.
--
-- Re-running is safe: the old tables are recognized by their server_id
-- column and only they are dropped. As in 9023, the DDL runs through EXECUTE
-- inside a DO block so sqlc does not see these tables.

DO $mcp_oauth$
DECLARE
    old_rows bigint := 0;
    n bigint;
BEGIN
    IF to_regclass('mcp_oauth_server') IS NOT NULL THEN
        EXECUTE 'SELECT count(*) FROM mcp_oauth_server' INTO n;
        old_rows := old_rows + n;
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'mcp_oauth_connection' AND column_name = 'server_id'
    ) THEN
        EXECUTE 'SELECT count(*) FROM mcp_oauth_connection' INTO n;
        old_rows := old_rows + n;
    END IF;
    IF old_rows > 0 THEN
        RAISE EXCEPTION 'mcp_oauth_server / mcp_oauth_connection hold % row(s); delete them (disconnect) before this migration', old_rows;
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'mcp_oauth_connection' AND column_name = 'server_id'
    ) THEN
        EXECUTE 'DROP TABLE mcp_oauth_connection';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'mcp_oauth_state' AND column_name = 'server_id'
    ) THEN
        EXECUTE 'DROP TABLE mcp_oauth_state';
    END IF;
    EXECUTE 'DROP TABLE IF EXISTS mcp_oauth_server';

    -- One user's grant for one entry. needs_reconnect keeps no token: the
    -- refresh token was rejected or lost, or the entry's URL changed, so only
    -- a new connect can bring it back.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS mcp_oauth_connection (
        server_ref                 TEXT NOT NULL
            CHECK (server_ref ~ '^(ws:[0-9a-f-]{36}|agent:[0-9a-f-]{36}:[A-Za-z0-9_-]{1,64})$'),
        user_id                    UUID NOT NULL,
        workspace_id               UUID NOT NULL,
        server_url                 TEXT NOT NULL CHECK (char_length(server_url) BETWEEN 9 AND 2048 AND server_url LIKE 'https://%'),
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
        PRIMARY KEY (server_ref, user_id),
        CHECK (status <> 'connected' OR access_token_ciphertext IS NOT NULL)
    )
    $ddl$;

    -- A started, not yet completed authorization; see 9023. The callback
    -- deletes the row as it reads it, so a state works once.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS mcp_oauth_state (
        state_hash                 BYTEA PRIMARY KEY CHECK (octet_length(state_hash) = 32),
        workspace_id               UUID NOT NULL,
        server_ref                 TEXT NOT NULL
            CHECK (server_ref ~ '^(ws:[0-9a-f-]{36}|agent:[0-9a-f-]{36}:[A-Za-z0-9_-]{1,64})$'),
        server_url                 TEXT NOT NULL CHECK (char_length(server_url) BETWEEN 9 AND 2048 AND server_url LIKE 'https://%'),
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
