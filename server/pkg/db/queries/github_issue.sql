-- Modified for the mouse9527/multica fork: GitHub-native issues.
-- Tables come from migration 9000; see it for column semantics.

-- ---------------------------------------------------------------------------
-- issue_external
-- ---------------------------------------------------------------------------

-- name: UpsertIssueExternal :one
-- The GitHub fields are refreshed only when the incoming snapshot is not older
-- than the stored one, so an out-of-order poll page cannot roll a row back.
-- The returned row is the stored state after the call.
INSERT INTO issue_external (
    issue_id, workspace_id, provider, external_repo_id, external_repo,
    external_number, external_node_id, state, state_reason, external_updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
ON CONFLICT (issue_id) DO UPDATE SET
    external_repo       = CASE WHEN EXCLUDED.external_updated_at >= issue_external.external_updated_at
                               THEN EXCLUDED.external_repo ELSE issue_external.external_repo END,
    state               = CASE WHEN EXCLUDED.external_updated_at >= issue_external.external_updated_at
                               THEN EXCLUDED.state ELSE issue_external.state END,
    state_reason        = CASE WHEN EXCLUDED.external_updated_at >= issue_external.external_updated_at
                               THEN EXCLUDED.state_reason ELSE issue_external.state_reason END,
    external_updated_at = GREATEST(EXCLUDED.external_updated_at, issue_external.external_updated_at),
    external_synced_at  = now(),
    updated_at          = now()
WHERE issue_external.workspace_id = EXCLUDED.workspace_id
RETURNING *;

-- name: GetIssueExternal :one
SELECT * FROM issue_external
WHERE issue_id = $1 AND workspace_id = $2;

-- name: GetIssueExternalByNumber :one
SELECT * FROM issue_external
WHERE workspace_id = $1
  AND provider = $2
  AND external_repo_id = $3
  AND external_number = $4;

-- name: GetIssueExternalByRepoName :one
-- Resolves an "owner/repo#N" key. GitHub matches repository names without
-- regard to case; if a rename left two rows under one name, the most recently
-- synced one is the repository that holds the name now.
SELECT * FROM issue_external
WHERE workspace_id = sqlc.arg('workspace_id')
  AND provider = sqlc.arg('provider')
  AND lower(external_repo) = lower(sqlc.arg('external_repo'))
  AND external_number = sqlc.arg('external_number')
ORDER BY external_synced_at DESC
LIMIT 1;

-- name: ListIssueExternalsByIssueIDs :many
-- Batch lookup for list/identifier rendering. Issues missing from the result
-- are local.
SELECT * FROM issue_external
WHERE workspace_id = sqlc.arg('workspace_id')
  AND issue_id = ANY(sqlc.arg('issue_ids')::uuid[]);

-- name: DeleteIssueExternal :exec
DELETE FROM issue_external
WHERE issue_id = $1 AND workspace_id = $2;

-- ---------------------------------------------------------------------------
-- github_repo_sync
-- ---------------------------------------------------------------------------

-- name: UpsertGithubRepoSync :one
-- Registers a repository for polling, or refreshes its owner/name after a
-- rename. Cursors and status are left untouched on conflict.
INSERT INTO github_repo_sync (workspace_id, repo_id, owner, name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, repo_id) DO UPDATE SET
    owner      = EXCLUDED.owner,
    name       = EXCLUDED.name,
    updated_at = now()
RETURNING *;

-- name: GetGithubRepoSync :one
SELECT * FROM github_repo_sync
WHERE workspace_id = $1 AND repo_id = $2;

-- name: ListGithubRepoSyncsByWorkspace :many
SELECT * FROM github_repo_sync
WHERE workspace_id = $1
ORDER BY owner, name;

-- name: ListActiveGithubRepoSyncs :many
-- Poller entry point across workspaces. The table holds one row per
-- registered repository, so a scan is cheap.
SELECT * FROM github_repo_sync
WHERE status = 'active'
ORDER BY last_polled_at ASC NULLS FIRST, workspace_id, repo_id;

-- name: AdvanceGithubRepoSyncCursors :one
-- A NULL argument keeps that cursor; cursors never move backwards.
UPDATE github_repo_sync SET
    issues_cursor   = GREATEST(issues_cursor, sqlc.narg('issues_cursor')::timestamptz),
    comments_cursor = GREATEST(comments_cursor, sqlc.narg('comments_cursor')::timestamptz),
    pulls_cursor    = GREATEST(pulls_cursor, sqlc.narg('pulls_cursor')::timestamptz),
    last_polled_at  = now(),
    updated_at      = now()
WHERE workspace_id = sqlc.arg('workspace_id') AND repo_id = sqlc.arg('repo_id')
RETURNING *;

-- name: MarkGithubRepoSyncReconciled :exec
UPDATE github_repo_sync SET
    last_full_reconcile_at = $3,
    updated_at             = now()
WHERE workspace_id = $1 AND repo_id = $2;

-- name: SetGithubRepoSyncStatus :exec
UPDATE github_repo_sync SET
    status     = $3,
    last_error = $4,
    updated_at = now()
WHERE workspace_id = $1 AND repo_id = $2;

-- name: DeleteGithubRepoSync :exec
DELETE FROM github_repo_sync
WHERE workspace_id = $1 AND repo_id = $2;

-- ---------------------------------------------------------------------------
-- github_write_intent
-- ---------------------------------------------------------------------------

-- name: CreateGithubWriteIntent :one
INSERT INTO github_write_intent (
    workspace_id, op, repo_id, external_repo, issue_id, external_number,
    comment_id, payload_digest, actor_type, actor_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
) RETURNING *;

-- name: GetGithubWriteIntent :one
SELECT * FROM github_write_intent
WHERE id = $1 AND workspace_id = $2;

-- name: UpdateGithubWriteIntentState :one
-- Moves an intent between states. A NULL argument keeps the stored value, so
-- a later transition never erases the github_id or target learned earlier.
-- Settled intents (applied, failed) are final and are not updated.
UPDATE github_write_intent SET
    state           = sqlc.arg('state'),
    github_id       = COALESCE(sqlc.narg('github_id'), github_id),
    issue_id        = COALESCE(sqlc.narg('issue_id'), issue_id),
    external_number = COALESCE(sqlc.narg('external_number'), external_number),
    comment_id      = COALESCE(sqlc.narg('comment_id'), comment_id),
    error           = sqlc.narg('error'),
    updated_at      = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND state NOT IN ('applied', 'failed')
RETURNING *;

-- name: ListOpenGithubWriteIntents :many
-- Unsettled intents created at or before created_before, oldest first, for the
-- reconciler. Served by idx_github_write_intent_open.
SELECT * FROM github_write_intent
WHERE workspace_id = sqlc.arg('workspace_id')
  AND state IN ('pending', 'github_ok', 'unknown')
  AND created_at <= sqlc.arg('created_before')
ORDER BY created_at ASC
LIMIT sqlc.arg('row_limit');

-- ---------------------------------------------------------------------------
-- project_github_settings
-- ---------------------------------------------------------------------------

-- name: GetProjectGithubSettings :one
SELECT * FROM project_github_settings
WHERE project_id = $1 AND workspace_id = $2;

-- name: UpsertProjectGithubSettings :one
INSERT INTO project_github_settings (
    project_id, workspace_id, default_resource_id, status_label_map
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (project_id) DO UPDATE SET
    default_resource_id = EXCLUDED.default_resource_id,
    status_label_map    = EXCLUDED.status_label_map,
    updated_at          = now()
WHERE project_github_settings.workspace_id = EXCLUDED.workspace_id
RETURNING *;

-- name: SetProjectGithubDefaultResource :one
-- Sets only the default repository; status_label_map is kept.
INSERT INTO project_github_settings (
    project_id, workspace_id, default_resource_id
) VALUES (
    $1, $2, $3
)
ON CONFLICT (project_id) DO UPDATE SET
    default_resource_id = EXCLUDED.default_resource_id,
    updated_at          = now()
WHERE project_github_settings.workspace_id = EXCLUDED.workspace_id
RETURNING *;

-- name: DeleteProjectGithubSettings :exec
DELETE FROM project_github_settings
WHERE project_id = $1 AND workspace_id = $2;

-- ---------------------------------------------------------------------------
-- comment_external
-- ---------------------------------------------------------------------------

-- name: UpsertCommentExternal :one
-- Same freshness rule as UpsertIssueExternal: an older snapshot never
-- overwrites a newer one.
INSERT INTO comment_external (
    comment_id, workspace_id, external_id, login, avatar_url, external_updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (comment_id) DO UPDATE SET
    login               = CASE WHEN EXCLUDED.external_updated_at >= comment_external.external_updated_at
                               THEN EXCLUDED.login ELSE comment_external.login END,
    avatar_url          = CASE WHEN EXCLUDED.external_updated_at >= comment_external.external_updated_at
                               THEN EXCLUDED.avatar_url ELSE comment_external.avatar_url END,
    external_updated_at = GREATEST(EXCLUDED.external_updated_at, comment_external.external_updated_at),
    updated_at          = now()
WHERE comment_external.workspace_id = EXCLUDED.workspace_id
RETURNING *;

-- name: GetCommentExternal :one
SELECT * FROM comment_external
WHERE comment_id = $1 AND workspace_id = $2;

-- name: GetCommentExternalByExternalID :one
SELECT * FROM comment_external
WHERE workspace_id = $1 AND external_id = $2;

-- name: ListCommentExternalsByCommentIDs :many
SELECT * FROM comment_external
WHERE workspace_id = sqlc.arg('workspace_id')
  AND comment_id = ANY(sqlc.arg('comment_ids')::uuid[]);

-- name: DeleteCommentExternal :exec
DELETE FROM comment_external
WHERE comment_id = $1 AND workspace_id = $2;

-- ---------------------------------------------------------------------------
-- Workspace teardown
-- ---------------------------------------------------------------------------

-- name: DeleteWorkspaceGithubIssueData :exec
-- Called from the workspace delete transaction. None of these tables has a
-- foreign key, so every row is removed explicitly here.
WITH deleted_issue_externals AS (
    DELETE FROM issue_external WHERE workspace_id = $1
),
deleted_comment_externals AS (
    DELETE FROM comment_external WHERE workspace_id = $1
),
deleted_write_intents AS (
    DELETE FROM github_write_intent WHERE workspace_id = $1
),
deleted_project_settings AS (
    DELETE FROM project_github_settings WHERE workspace_id = $1
)
DELETE FROM github_repo_sync WHERE github_repo_sync.workspace_id = $1;
