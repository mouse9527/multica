-- Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).
--
-- One new table for the hold: an inbox item for the owner of a chat-bound
-- Router subscription, about an issue in its scope, that goes to the Router
-- before it goes to the inbox. Slice 2 runs the hold in shadow mode only: the
-- item is written to the inbox as always, and a 'shadow' row records that the
-- hold would have taken it and how long deciding took. Slice 3 holds for real
-- with the same table ('held' -> 'releasing' -> 'released', or 'handled'), so
-- the columns it needs are here already and it adds no ALTER.
--
-- inbox_item_id is the item's id, chosen before the insert; a shadow row
-- whose item never landed (ineligible recipient, rolled-back transaction) is
-- told apart by joining inbox_item. item is the full inbox write, so a held
-- item can later be written exactly as it would have been.
--
-- No foreign keys (repo rule). Secondary indexes are built concurrently in
-- 9016-9017; the table starts empty. As in 9004, the DDL runs through EXECUTE
-- inside a DO block so sqlc does not see the table.

DO $router$
BEGIN
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_hold (
        id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id        UUID NOT NULL,
        subscription_id     UUID NOT NULL,
        agent_id            UUID NOT NULL,
        recipient_user_id   UUID NOT NULL,
        inbox_item_id       UUID NOT NULL,
        issue_id            UUID NOT NULL,
        item_type           TEXT NOT NULL,
        severity            TEXT NOT NULL,
        title               TEXT NOT NULL,
        actor_type          TEXT,
        actor_id            UUID,
        item                JSONB NOT NULL,
        mode                TEXT NOT NULL CHECK (mode IN ('shadow', 'hold')),
        status              TEXT NOT NULL CHECK (status IN ('shadow', 'held', 'releasing', 'released', 'handled')),
        decide_micros       INTEGER NOT NULL,
        dispatch_task_id    UUID,
        deadline_at         TIMESTAMPTZ,
        decided_at          TIMESTAMPTZ,
        decided_by_agent_id UUID,
        decided_by_task_id  UUID,
        decision_note       TEXT,
        created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;
END
$router$;
