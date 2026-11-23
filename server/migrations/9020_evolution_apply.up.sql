-- Added by the mouse9527/multica fork: Evolution P4, apply and rollback
-- (M-138).
--
-- Two new tables; no existing table is altered. No foreign keys (repo rule):
-- evolution.DeleteWorkspaceData removes the rows with the workspace. As in
-- 9019 the DDL runs through EXECUTE inside a DO block so sqlc does not see
-- these tables. Both stay small (one row per proposal, one per apply or
-- rollback), so they have no secondary indexes.

DO $evolution$
BEGIN
    -- What a proposal changes, as the server checked it when the proposal
    -- was filed. Apply reads the change from here, never from the issue body
    -- or metadata, which members and agents can edit. Written once, never
    -- updated.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS evolution_proposal (
        issue_id       UUID PRIMARY KEY,
        workspace_id   UUID NOT NULL,
        run_id         UUID NOT NULL,
        target_type    TEXT NOT NULL CHECK (target_type IN ('agent', 'workflow', 'squad')),
        target_id      UUID NOT NULL,
        field          TEXT NOT NULL,
        base_hash      TEXT NOT NULL,
        change_before  TEXT NOT NULL,
        change_after   TEXT NOT NULL,
        -- The field as it was when the proposal was filed, and as the change
        -- leaves it.
        base_value     TEXT NOT NULL,
        proposed_value TEXT NOT NULL,
        created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;

    -- The version history of the agents and workflows Evolution changed:
    -- one row per apply and per rollback, append-only. A rollback row points
    -- at the apply it undoes (reverted_of). The snapshots hold every
    -- whitelisted field of the target, before and after the write; the
    -- hashes are of those snapshots.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS evolution_config_version (
        id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id      UUID NOT NULL,
        target_type       TEXT NOT NULL CHECK (target_type IN ('agent', 'workflow')),
        target_id         UUID NOT NULL,
        version           INTEGER NOT NULL,
        action            TEXT NOT NULL CHECK (action IN ('apply', 'rollback')),
        field             TEXT NOT NULL,
        proposal_issue_id UUID NOT NULL,
        reverted_of       UUID,
        before_hash       TEXT NOT NULL,
        after_hash        TEXT NOT NULL,
        before_snapshot   JSONB NOT NULL,
        after_snapshot    JSONB NOT NULL,
        actor_id          UUID NOT NULL,
        created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
        CHECK ((action = 'rollback') = (reverted_of IS NOT NULL))
    )
    $ddl$;
END
$evolution$;
