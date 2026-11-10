-- Modified for the mouse9527/multica fork: GitHub-native issues.
--
-- Five new tables back GitHub-native issues. No existing table is altered: an
-- issue is GitHub-backed exactly when it has an issue_external row, and every
-- issue without one stays local with unchanged behavior.
--
-- No foreign keys (repo rule): workspace teardown deletes these rows through
-- DeleteWorkspaceGithubIssueData, and issue/comment deletion paths remove their
-- side rows in application code. Each table has a workspace_id so teardown and
-- workspace-scoped reads need no join. Secondary indexes are built
-- concurrently in 9002-9004; the tables start empty, so the window between
-- this migration and those indexes carries no load.
--
-- GitHub ids (repository, comment) are BIGINT: they are int64 in the REST API.

-- The GitHub identity of an issue. external_repo is the current "owner/repo"
-- display name; external_repo_id stays stable across renames and transfers.
-- state and state_reason are GitHub's raw values, kept apart from the Multica
-- status so the mapping can be re-derived.
CREATE TABLE IF NOT EXISTS issue_external (
    issue_id            UUID PRIMARY KEY,
    workspace_id        UUID NOT NULL,
    provider            TEXT NOT NULL CHECK (provider IN ('github')),
    external_repo_id    BIGINT NOT NULL,
    external_repo       TEXT NOT NULL,
    external_number     INTEGER NOT NULL,
    external_node_id    TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('open', 'closed')),
    state_reason        TEXT,
    external_updated_at TIMESTAMPTZ NOT NULL,
    external_synced_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Poll state per registered repository. Each cursor is the largest GitHub
-- updated_at seen for that stream; NULL means the stream has not been read.
CREATE TABLE IF NOT EXISTS github_repo_sync (
    workspace_id           UUID NOT NULL,
    repo_id                BIGINT NOT NULL,
    owner                  TEXT NOT NULL,
    name                   TEXT NOT NULL,
    issues_cursor          TIMESTAMPTZ,
    comments_cursor        TIMESTAMPTZ,
    pulls_cursor           TIMESTAMPTZ,
    last_full_reconcile_at TIMESTAMPTZ,
    status                 TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'error')),
    last_error             TEXT,
    last_polled_at         TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, repo_id)
);

-- A write to GitHub, recorded before the request is sent. id is the
-- idempotency key and is embedded in the written body as
-- <!-- multica:op=<id> -->. State machine:
--   pending   -> request not yet answered
--   github_ok -> GitHub accepted it (github_id set); local write not confirmed
--   applied   -> GitHub and the local row agree
--   unknown   -> timeout, disconnect or 5xx; never retried automatically
--   failed    -> GitHub rejected it, or an unknown write was given up
-- Target columns are NULL until known (create_issue has no issue yet).
-- actor_type/actor_id name who asked for the write; the poller restores
-- authorship from this row, never from the marker alone.
CREATE TABLE IF NOT EXISTS github_write_intent (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL,
    op              TEXT NOT NULL CHECK (op IN (
        'create_issue', 'update_issue',
        'create_comment', 'update_comment', 'delete_comment'
    )),
    repo_id         BIGINT NOT NULL,
    external_repo   TEXT NOT NULL,
    issue_id        UUID,
    external_number INTEGER,
    comment_id      UUID,
    payload_digest  TEXT NOT NULL,
    actor_type      TEXT NOT NULL CHECK (actor_type IN ('member', 'agent')),
    actor_id        UUID NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'github_ok', 'applied', 'unknown', 'failed')),
    github_id       BIGINT,
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GitHub settings of a project. default_resource_id points at one of the
-- project's project_resource rows of type github_repo. status_label_map maps
-- a project status key to the "status:<key>" label written on GitHub.
CREATE TABLE IF NOT EXISTS project_github_settings (
    project_id          UUID PRIMARY KEY,
    workspace_id        UUID NOT NULL,
    default_resource_id UUID,
    status_label_map    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The GitHub identity of a comment mirrored from or written to GitHub, with
-- the GitHub author shown for comments that have no Multica author.
CREATE TABLE IF NOT EXISTS comment_external (
    comment_id          UUID PRIMARY KEY,
    workspace_id        UUID NOT NULL,
    external_id         BIGINT NOT NULL,
    login               TEXT NOT NULL,
    avatar_url          TEXT,
    external_updated_at TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
