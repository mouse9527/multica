-- Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).
--
-- Three new tables capture issue events for agents that subscribe to an issue
-- tree or a project. No existing table is altered. Every table carries
-- workspace_id so workspace teardown needs no join.
--
-- No foreign keys (repo rule): workspace teardown and subscription deletion
-- remove dependent rows in application code. Secondary indexes are built
-- concurrently in 9005-9007; the tables start empty.
--
-- The DDL runs through EXECUTE inside a DO block so sqlc, which reads this
-- directory as its schema, does not see these tables. The Router package uses
-- hand-written SQL only, and a sqlc-visible table would add structs to the
-- shared pkg/db/generated/models.go that every upstream sync touches.

DO $router$
BEGIN
    -- One agent watching one scope. An agent may hold several subscriptions; when
    -- scopes overlap, an event goes only to the agent's narrowest one.
    --
    -- anchor_issue_id, debounce_seconds, max_wait_seconds, max_runs_per_hour,
    -- state and state_revision belong to dispatch (slice S2). S1 stores their
    -- defaults and never reads them, so S2 needs no ALTER.
    --
    -- active_since is when capture last started (creation or resume): nothing
    -- older is captured, by the listener or by reconcile. reconcile_cursor is the
    -- upper bound of the last reconcile pass.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_subscription (
        id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id      UUID NOT NULL,
        agent_id          UUID NOT NULL,
        owner_user_id     UUID,
        scope_type        TEXT NOT NULL CHECK (scope_type IN ('issue_tree', 'project')),
        scope_id          UUID NOT NULL,
        anchor_issue_id   UUID,
        event_types       TEXT[] NOT NULL,
        debounce_seconds  INTEGER NOT NULL DEFAULT 60,
        max_wait_seconds  INTEGER NOT NULL DEFAULT 600,
        max_runs_per_hour INTEGER NOT NULL DEFAULT 6,
        enabled           BOOLEAN NOT NULL DEFAULT true,
        paused_reason     TEXT CHECK (paused_reason IN ('rate', 'manual', 'agent_unavailable')),
        state             TEXT NOT NULL DEFAULT '',
        state_revision    BIGINT NOT NULL DEFAULT 0,
        active_since      TIMESTAMPTZ NOT NULL DEFAULT now(),
        reconcile_cursor  TIMESTAMPTZ,
        created_by_type   TEXT NOT NULL CHECK (created_by_type IN ('member', 'agent')),
        created_by_id     UUID NOT NULL,
        created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;

    -- The durable event queue. event_key is unique per agent, so the listener and
    -- reconcile can both offer the same change and only one row lands. source
    -- records who wrote it: a 'reconcile' row is one the listener missed, which is
    -- what the capture_miss metric counts. payload holds references and changed
    -- field names only, never comment bodies. consumed_task_id is set by dispatch
    -- (slice S2).
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_event (
        id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id     UUID NOT NULL,
        subscription_id  UUID NOT NULL,
        agent_id         UUID NOT NULL,
        event_key        TEXT NOT NULL,
        kind             TEXT NOT NULL CHECK (kind IN ('issue_created', 'status_changed', 'assignee_changed', 'comment_created', 'task_failed')),
        issue_id         UUID NOT NULL,
        actor_type       TEXT,
        actor_id         UUID,
        source           TEXT NOT NULL CHECK (source IN ('bus', 'reconcile')),
        is_self          BOOLEAN NOT NULL DEFAULT false,
        payload          JSONB NOT NULL DEFAULT '{}'::jsonb,
        occurred_at      TIMESTAMPTZ NOT NULL,
        consumed_task_id UUID,
        created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;

    -- The last issue state a subscription has seen. Status and assignee are
    -- levels, not edges: reconcile compares the current row with this one and
    -- emits an event only for a difference the listener did not record.
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_issue_seen (
        subscription_id UUID NOT NULL,
        issue_id        UUID NOT NULL,
        workspace_id    UUID NOT NULL,
        status          TEXT NOT NULL,
        assignee_type   TEXT,
        assignee_id     UUID,
        seen_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
        PRIMARY KEY (subscription_id, issue_id)
    )
    $ddl$;
END
$router$;
