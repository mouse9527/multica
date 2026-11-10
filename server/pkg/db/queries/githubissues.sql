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
