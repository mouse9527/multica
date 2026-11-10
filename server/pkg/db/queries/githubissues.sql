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

-- name: LockCommentExternalByExternalID :one
-- Serializes syncs of one GitHub comment, so a refresh compares and rewrites
-- the stored text against the newest snapshot only.
SELECT * FROM comment_external
WHERE workspace_id = $1 AND external_id = $2
FOR UPDATE;

-- name: DiscardExternalCommentWakeupReceipts :execrows
-- A comment written on GitHub by someone outside the workspace must not wake
-- an agent (docs/github-issues.md). The comment trigger records a receipt for
-- each event subscription on the issue; this removes the receipts that only
-- this comment produced, inside the transaction that wrote it, before any
-- dispatcher can see them. A receipt merged with an earlier pending event
-- (coalesced_count > 1) wakes for that earlier event and is kept.
DELETE FROM issue_wakeup_receipt
WHERE wakeup_id IN (SELECT w.id FROM issue_wakeup w WHERE w.issue_id = @issue_id)
  AND processed_at IS NULL
  AND task_id IS NULL
  AND event_type IN ('comment.created', 'comment.updated')
  AND payload->>'comment_id' = @comment_id::text
  AND COALESCE((payload->>'coalesced_count')::bigint, 1) = 1;

-- name: GetGitHubSyncDefaultOwner :one
-- The default creator of issues opened on GitHub by people outside the
-- workspace: the workspace's earliest owner.
SELECT user_id FROM member
WHERE workspace_id = $1 AND role = 'owner'
ORDER BY created_at, user_id
LIMIT 1;
