-- Added by the mouse9527/multica fork: Evolution P2, the data channel, cursor
-- and daily schedule (M-136).
--
-- Two new tables; no existing table is altered. Every table carries
-- workspace_id so workspace teardown needs no join. No foreign keys (repo
-- rule): evolution.DeleteWorkspaceData removes the rows with the workspace.
--
-- The DDL runs through EXECUTE inside a DO block so sqlc, which reads this
-- directory as its schema, does not see these tables (the Router convention,
-- migration 9004): the evolution package uses hand-written SQL only, so the
-- shared pkg/db/generated/models.go stays as upstream has it. The tables stay
-- small (a few rows per workspace, one run row per run), so they have no
-- secondary indexes.

DO $evolution$
BEGIN
    -- How far each source has been analysed, as a keyset (ts, item_id) over
    -- the source's (created_at, id). item_id NULL means every row at or before
    -- ts was read. A run reads from here; the position moves only when the
    -- run that read past it succeeded.
    --
    -- source 'member_notice' is the same kind of cursor over member join times:
    -- members who joined after ts have not been told that Evolution reads chats.
    --
    -- A 'chat' row exists only while chat reading is on and members were told;
    -- turning chat off deletes it, and turning it on again starts from now.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS evolution_cursor (
        workspace_id UUID NOT NULL,
        source       TEXT NOT NULL CHECK (source IN ('comment', 'chat', 'member_notice')),
        ts           TIMESTAMPTZ NOT NULL,
        item_id      UUID,
        updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
        PRIMARY KEY (workspace_id, source)
    )
    $ddl$;

    -- One analysis run: the issue the scheduler created for the Evolution
    -- agent, the window it may read up to, and how far it was served per
    -- source. At most one run per workspace is open at a time.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS evolution_run (
        id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id      UUID NOT NULL,
        agent_id          UUID NOT NULL,
        issue_id          UUID,
        kind              TEXT NOT NULL CHECK (kind IN ('scheduled', 'catch_up')),
        slot              DATE,
        status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'succeeded', 'failed')),
        window_until      TIMESTAMPTZ NOT NULL,
        comment_served_ts TIMESTAMPTZ,
        comment_served_id UUID,
        chat_served_ts    TIMESTAMPTZ,
        chat_served_id    UUID,
        served_items      INTEGER NOT NULL DEFAULT 0,
        failure_reason    TEXT,
        created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
        finished_at       TIMESTAMPTZ
    )
    $ddl$;
END
$evolution$;
