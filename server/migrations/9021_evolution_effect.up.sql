-- Added by the mouse9527/multica fork: Evolution P5, effect measurement
-- (M-139).
--
-- One new table; no existing table is altered. No foreign keys (repo rule):
-- evolution.DeleteWorkspaceData removes the rows with the workspace. As in
-- 9019 the DDL runs through EXECUTE inside a DO block so sqlc does not see
-- the table. It stays small (at most one row per apply), so it has no
-- secondary indexes.

DO $evolution$
BEGIN
    -- What an apply did to its target: the target's metrics over the
    -- window_days before the apply and the window_days after it, and the
    -- verdict drawn from them. One row per apply version, written once when
    -- the after window has passed. A regression is only reported (notified);
    -- nothing is rolled back.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS evolution_effect (
        version_id        UUID PRIMARY KEY,
        workspace_id      UUID NOT NULL,
        proposal_issue_id UUID NOT NULL,
        target_type       TEXT NOT NULL CHECK (target_type IN ('agent', 'workflow')),
        target_id         UUID NOT NULL,
        applied_at        TIMESTAMPTZ NOT NULL,
        window_days       INTEGER NOT NULL CHECK (window_days > 0),
        sample_before     INTEGER NOT NULL,
        sample_after      INTEGER NOT NULL,
        metrics_before    JSONB NOT NULL,
        metrics_after     JSONB NOT NULL,
        -- Per compared metric: before, after, and whether it got better or
        -- worse beyond the threshold.
        findings          JSONB NOT NULL,
        verdict           TEXT NOT NULL CHECK (verdict IN
                              ('improved', 'unchanged', 'regressed', 'insufficient_data', 'not_measured')),
        -- Why a verdict is not_measured.
        reason            TEXT,
        -- Whether owners were told of a regression.
        notified          BOOLEAN NOT NULL DEFAULT false,
        evaluated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;
END
$evolution$;
