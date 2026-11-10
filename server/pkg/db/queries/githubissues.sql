-- Added by the mouse9527/multica fork: queries of GitHub-native issues
-- (server/internal/githubissues). Upstream has no such queries.

-- name: ListGitHubTokenExpiryRecipients :many
-- Owners and admins of every workspace with a GitHub repository registered on
-- one of its projects: the workspaces whose GitHub issues depend on the token.
SELECT DISTINCT m.workspace_id, m.user_id
FROM member m
WHERE m.role IN ('owner', 'admin')
  AND EXISTS (
      SELECT 1 FROM project_resource pr
      WHERE pr.workspace_id = m.workspace_id
        AND pr.resource_type = 'github_repo'
  )
ORDER BY m.workspace_id, m.user_id;

-- name: GitHubTokenExpiryReminderSent :one
-- Whether this member already got the reminder for this expiry date and
-- threshold. Archived and read reminders count: each is sent once.
SELECT EXISTS (
    SELECT 1 FROM inbox_item
    WHERE workspace_id = @workspace_id
      AND recipient_type = 'member'
      AND recipient_id = @recipient_id
      AND type = 'github_token_expiry'
      AND details->>'expires_at' = @expires_at::text
      AND details->>'threshold_days' = @threshold_days::text
);

-- name: ListWorkspaceGitHubRepoRefs :many
-- The github_repo resources registered on any project of the workspace: the
-- allowlist behind githubissues.RepoClient. Read-only on project_resource.
SELECT resource_ref FROM project_resource
WHERE workspace_id = $1
  AND resource_type = 'github_repo'
ORDER BY created_at, id;

-- name: InsertGithubRepoPin :one
-- Pins a repository id to the first name RepoClient resolved it from. Returns
-- no row when another caller pinned the id first; RepoClient then re-reads
-- the pins instead of overwriting them.
INSERT INTO github_repo_sync (workspace_id, repo_id, owner, name)
VALUES (@workspace_id, @repo_id, @owner, @name)
ON CONFLICT (workspace_id, repo_id) DO NOTHING
RETURNING *;

-- name: MoveGithubRepoPin :one
-- Moves a pin to a new name only if it still has the name RepoClient read
-- (compare-and-swap). Returns no row when the pin changed in between.
UPDATE github_repo_sync SET
    owner      = @owner,
    name       = @name,
    updated_at = now()
WHERE workspace_id = @workspace_id
  AND repo_id = @repo_id
  AND owner = @from_owner
  AND name = @from_name
RETURNING *;
