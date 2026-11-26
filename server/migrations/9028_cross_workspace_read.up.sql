-- Added by the mouse9527/multica fork: cross-workspace read-only access for
-- agents (M-307, GitHub #207).
--
-- Two new tables; no existing table is altered. No foreign keys (repo rule).
-- Their indexes are built concurrently by 9029 and 9030.
--
-- The DDL runs through EXECUTE inside a DO block so sqlc, which reads this
-- directory as its schema, does not see these tables (the convention of
-- migrations 9004 and 9023): the crossread package uses hand-written SQL only,
-- so the shared pkg/db/generated/models.go stays as upstream has it.

DO $cross_workspace_read$
BEGIN
    -- A target workspace's owner/admin lets agents of a source workspace read
    -- the target. agent_id NULL means every agent of the source workspace.
    -- A grant is never deleted: revoking sets revoked_at, so the audit rows
    -- keep pointing at a row that says who granted what.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS cross_workspace_read_grant (
        id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        target_workspace_id UUID NOT NULL,
        source_workspace_id UUID NOT NULL,
        agent_id            UUID,
        note                TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
        created_by          UUID NOT NULL,
        created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
        expires_at          TIMESTAMPTZ,
        revoked_at          TIMESTAMPTZ,
        revoked_by          UUID,
        CHECK (target_workspace_id <> source_workspace_id),
        CHECK (expires_at IS NULL OR expires_at > created_at),
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
    )
    $ddl$;

    -- One row per cross-workspace request a grant matched, written before the
    -- request is served. route is the allowlist pattern that admitted the
    -- request ("GET /api/issues/*"), or the method and path of a refused one;
    -- never a query string or a body.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS cross_workspace_read_audit (
        id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        grant_id            UUID NOT NULL,
        target_workspace_id UUID NOT NULL,
        source_workspace_id UUID NOT NULL,
        agent_id            UUID NOT NULL,
        task_id             UUID NOT NULL,
        method              TEXT NOT NULL CHECK (char_length(method) BETWEEN 1 AND 16),
        route               TEXT NOT NULL CHECK (char_length(route) BETWEEN 1 AND 512),
        allowed             BOOLEAN NOT NULL,
        created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;
END
$cross_workspace_read$;
