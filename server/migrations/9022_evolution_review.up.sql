-- Added by the mouse9527/multica fork: Evolution P6, the monthly condense
-- review (M-140).
--
-- One new table; no existing table is altered. No foreign keys (repo rule):
-- evolution.DeleteWorkspaceData removes the rows with the workspace. As in
-- 9019 the DDL runs through EXECUTE inside a DO block so sqlc does not see
-- the table. It stays small (one row per workspace per month), so it has no
-- secondary indexes; one review per workspace, kind, and month is kept by
-- the scheduler under a per-workspace advisory lock.

DO $evolution$
BEGIN
    -- A monthly review: the issue the scheduler created for the Evolution
    -- agent and the agents it asks the agent to condense, as measured when
    -- the review opened. 'skipped' is a month in which no agent met a
    -- threshold, so no issue was created. While a review is open, the
    -- Evolution agent's task on its issue may file condense proposals for
    -- the listed agents.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS evolution_review (
        id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id   UUID NOT NULL,
        agent_id       UUID NOT NULL,
        issue_id       UUID,
        kind           TEXT NOT NULL CHECK (kind IN ('condense')),
        -- The first day of the month the review is for.
        period         DATE NOT NULL,
        status         TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'succeeded', 'failed', 'skipped')),
        candidates     JSONB NOT NULL DEFAULT '[]'::jsonb,
        failure_reason TEXT,
        created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
        finished_at    TIMESTAMPTZ
    )
    $ddl$;
END
$evolution$;
