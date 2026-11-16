-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
--
-- Three new tables for dispatch and the owner notice outlet. No existing table
-- is altered; router_subscription already carries the S2 columns (anchor,
-- batching parameters, state) since 9004. Every table carries workspace_id so
-- workspace teardown needs no join.
--
-- No foreign keys (repo rule). Secondary indexes are built concurrently in
-- 9009-9013; the tables start empty. As in 9004, the DDL runs through EXECUTE
-- inside a DO block so sqlc does not see these tables.

DO $router$
BEGIN
    -- One anchor run started for a batch of a subscription's events. The
    -- events point at the run through router_event.consumed_task_id. A row
    -- stays open (settled_at NULL) while the run, or a system retry of it, has
    -- not finished; an open row holds every other dispatch of the same agent.
    -- outcome 'failed' means the run and all its retries ended without
    -- completing, and the batch went back to the queue.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_dispatch (
        id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id    UUID NOT NULL,
        subscription_id UUID NOT NULL,
        agent_id        UUID NOT NULL,
        anchor_issue_id UUID NOT NULL,
        task_id         UUID NOT NULL,
        event_count     INTEGER NOT NULL,
        outcome         TEXT CHECK (outcome IN ('completed', 'failed')),
        settled_at      TIMESTAMPTZ,
        created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;

    -- Who changed a subscription, and on whose authority. authorized_by_user_id
    -- is the member whose rights allowed the change: the member themself, or,
    -- when an agent acted for another agent (via_agent_id), the human who
    -- started that agent's run (via_task_id). Rows outlive the subscription.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_subscription_change (
        id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id         UUID NOT NULL,
        subscription_id      UUID NOT NULL,
        action               TEXT NOT NULL CHECK (action IN ('create', 'update', 'pause', 'resume', 'delete', 'auto_pause')),
        actor_type           TEXT NOT NULL CHECK (actor_type IN ('member', 'agent', 'system')),
        actor_id             UUID,
        authorized_by_user_id UUID,
        via_agent_id         UUID,
        via_task_id          UUID,
        detail               JSONB NOT NULL DEFAULT '{}'::jsonb,
        created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;

    -- A channel-agnostic notice to a member ("notify owner"). The sender
    -- writes one row; adapters deliver it and record their result per
    -- channel in channel_states ({"inbox": {"state": "sent", "ref": ...}}).
    -- dedupe_key is unique per workspace so a repeated send is a no-op.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS owner_notice (
        id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id           UUID NOT NULL,
        recipient_user_id      UUID NOT NULL,
        title                  TEXT NOT NULL,
        body                   TEXT NOT NULL DEFAULT '',
        refs                   JSONB NOT NULL DEFAULT '[]'::jsonb,
        urgency                TEXT NOT NULL DEFAULT 'normal' CHECK (urgency IN ('normal', 'high')),
        dedupe_key             TEXT,
        sender_type            TEXT NOT NULL CHECK (sender_type IN ('member', 'agent', 'system')),
        sender_id              UUID,
        source_subscription_id UUID,
        source_task_id         UUID,
        channel_states         JSONB NOT NULL DEFAULT '{}'::jsonb,
        created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;
END
$router$;
