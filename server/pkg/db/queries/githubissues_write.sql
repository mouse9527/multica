-- Added by the mouse9527/multica fork: queries of the GitHub write path
-- (server/internal/githubissues/write*.go). Upstream has no such queries.
-- Only fork tables are written; upstream tables are only read.

-- name: InsertGithubWriteIntent :one
-- Records a write before it is sent. The id is chosen by the caller because
-- it goes into the body as the op marker, and the digest is taken over that
-- body.
INSERT INTO github_write_intent (
    id, workspace_id, op, repo_id, external_repo, issue_id, external_number,
    comment_id, payload_digest, actor_type, actor_id, github_id
) VALUES (
    @id, @workspace_id, @op, @repo_id, @external_repo, sqlc.narg('issue_id'),
    sqlc.narg('external_number'), sqlc.narg('comment_id'), @payload_digest,
    @actor_type, @actor_id, sqlc.narg('github_id')
) RETURNING *;

-- name: BumpIssueExternalUpdatedAt :exec
-- Records the updated_at GitHub returned for a write before the local copy
-- follows, so a poll page read before the write cannot roll the issue back.
-- The GitHub state columns are left alone: if the local write then fails, the
-- next poll still sees the state change and heals the issue.
UPDATE issue_external SET
    external_updated_at = GREATEST(external_updated_at, @external_updated_at::timestamptz),
    updated_at          = now()
WHERE issue_id = @issue_id AND workspace_id = @workspace_id;

-- name: ListGithubWriteIntentsToReconcile :many
-- Intents of one repository that GitHub never answered (pending, unknown),
-- or that GitHub accepted without a confirmed local copy (github_ok), created
-- at or before created_before, oldest first. Intents already handed to a
-- person (error 'needs_confirmation: ...') are not checked again. Served by
-- idx_github_write_intent_open.
SELECT * FROM github_write_intent
WHERE workspace_id = sqlc.arg('workspace_id')
  AND repo_id = sqlc.arg('repo_id')
  AND state IN ('pending', 'github_ok', 'unknown')
  AND created_at <= sqlc.arg('created_before')
  AND (error IS NULL OR error NOT LIKE 'needs_confirmation:%')
ORDER BY created_at ASC
LIMIT sqlc.arg('row_limit');
