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

-- name: ListGitHubRepoRefs :many
-- The github_repo resources registered on any project of any workspace: the
-- allowlist behind githubissues.Service.SnapshotTransport, which serves the
-- whole server. Read-only on project_resource.
SELECT resource_ref FROM project_resource
WHERE resource_type = 'github_repo';

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

-- name: ListGitHubRepoResources :many
-- Every github_repo resource of every workspace, for the poller: each
-- registered repository is polled once per workspace that registers it, and
-- issues opened on GitHub land in the project that registered it first.
-- Read-only on project_resource.
SELECT workspace_id, project_id, resource_ref FROM project_resource
WHERE resource_type = 'github_repo'
ORDER BY workspace_id, created_at, id;

-- name: ListWorkspaceGitHubRepoResources :many
-- The github_repo resources of one workspace with their projects, oldest
-- first: an imported GitHub issue lands in the project that registered its
-- repository first, as the poller's did. Read-only on project_resource.
SELECT project_id, resource_ref FROM project_resource
WHERE workspace_id = $1
  AND resource_type = 'github_repo'
ORDER BY created_at, id;

-- name: ListAllGithubRepoSyncs :many
-- Every pin with its poll state, across workspaces, for the poller.
SELECT * FROM github_repo_sync
ORDER BY last_polled_at ASC NULLS FIRST, workspace_id, repo_id;

-- name: ListFollowedGithubRepos :many
-- The pinned repositories the poller follows: those with a tracked issue the
-- reconcile has not found deleted or transferred away, or with a write from
-- Multica still to settle (the reconciler runs inside the poll, so an
-- unanswered create in a repository with nothing tracked yet still gets
-- settled or handed over). Repositories with neither get no request at all,
-- so the poll's requests follow the tracked issues, not the registered
-- repositories. Read-only.
SELECT s.workspace_id, s.repo_id FROM github_repo_sync s
WHERE EXISTS (
        SELECT 1 FROM issue_external x
        WHERE x.workspace_id = s.workspace_id
          AND x.provider = 'github'
          AND x.external_repo_id = s.repo_id
          AND (x.state_reason IS NULL OR x.state_reason NOT LIKE 'multica:%')
    )
   OR EXISTS (
        SELECT 1 FROM github_write_intent i
        WHERE i.workspace_id = s.workspace_id
          AND i.repo_id = s.repo_id
          AND i.state IN ('pending', 'github_ok', 'unknown')
          AND (i.error IS NULL OR i.error NOT LIKE 'needs_confirmation:%')
    );

-- name: FinishGithubRepoPoll :exec
-- Records the outcome of one poll of a repository: last_error is NULL after a
-- clean poll. status is left alone, so a paused repository stays paused.
UPDATE github_repo_sync SET
    last_error     = sqlc.narg('last_error'),
    last_polled_at = now(),
    updated_at     = now()
WHERE workspace_id = @workspace_id AND repo_id = @repo_id;

-- name: UpdateGitHubIssueContent :one
-- Rewrites the title and description of a GitHub-backed issue from GitHub,
-- the only writer of both. Returns no row when neither changed, so a re-poll
-- of the same snapshot writes nothing and bumps no revision.
UPDATE issue SET
    title            = @title,
    description      = sqlc.narg('description'),
    revision         = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at       = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND (title IS DISTINCT FROM @title OR description IS DISTINCT FROM sqlc.narg('description'))
RETURNING *;

-- name: ListTrackedGitHubIssuesInRepo :many
-- The GitHub issues of one repository that the workspace tracks (imported, or
-- opened from Multica), by number: the issues whose linked pull requests the
-- poller looks for. has_unmerged_pull is whether a pull request the poller
-- mirrored (installation_id) and has not seen merged is linked to the issue:
-- its timeline is then read even while the issue is closed. Issues the
-- reconcile found deleted or transferred away (state_reason 'multica:...')
-- have no timeline left to read. Read-only.
SELECT x.issue_id, x.external_number, x.state, x.external_updated_at,
    EXISTS (
        SELECT 1 FROM issue_pull_request ipr
        JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
        WHERE ipr.issue_id = x.issue_id
          AND pr.workspace_id = x.workspace_id
          AND pr.installation_id = @installation_id
          AND pr.state <> 'merged'
    ) AS has_unmerged_pull
FROM issue_external x
WHERE x.workspace_id = @workspace_id
  AND x.provider = 'github'
  AND x.external_repo_id = @external_repo_id
  AND (x.state_reason IS NULL OR x.state_reason NOT LIKE 'multica:%')
ORDER BY x.external_number;

-- name: ListGitHubIssuesToReconcile :many
-- The tracked issues of one repository due for the daily reconcile: not
-- synced in the last check_seconds, and not already found deleted or
-- transferred away. Oldest sync first, so every check moves the next one
-- forward. external_synced_at is stamped by the database clock (see
-- UpsertIssueExternal), so the cutoff is too.
SELECT * FROM issue_external
WHERE workspace_id = @workspace_id
  AND provider = 'github'
  AND external_repo_id = @external_repo_id
  AND external_synced_at < now() - make_interval(secs => @check_seconds::float8)
  AND (state_reason IS NULL OR state_reason NOT LIKE 'multica:%')
ORDER BY external_synced_at, issue_id
LIMIT @max_rows;

-- name: TouchIssueExternalSynced :exec
-- Records that the reconcile read the issue from GitHub, also when the
-- snapshot changed nothing (fork table only).
UPDATE issue_external SET external_synced_at = now()
WHERE issue_id = @issue_id AND workspace_id = @workspace_id;

-- name: MarkGitHubIssueGone :execrows
-- Marks a tracked issue deleted or transferred away on GitHub. state_reason
-- carries the mark ('multica:deleted', 'multica:transferred'): GitHub's own
-- reasons never start with 'multica:', and the table is not altered. Only the
-- row still at repo and number is marked (fork table only).
UPDATE issue_external SET
    state_reason       = @state_reason,
    external_synced_at = now(),
    updated_at         = now()
WHERE issue_id = @issue_id
  AND workspace_id = @workspace_id
  AND external_repo_id = @external_repo_id
  AND external_number = @external_number;

-- name: MoveIssueExternal :execrows
-- Re-attaches a tracked issue to where GitHub transferred it. Only the row
-- still at the old repo and number moves; the unique index on (workspace,
-- provider, repo, number) refuses a target another issue already tracks
-- (fork table only).
UPDATE issue_external SET
    external_repo_id   = @to_repo_id,
    external_repo      = @to_repo,
    external_number    = @to_number,
    external_synced_at = now(),
    updated_at         = now()
WHERE issue_id = @issue_id
  AND workspace_id = @workspace_id
  AND external_repo_id = @from_repo_id
  AND external_number = @from_number;

-- name: ListProjectGitHubIssues :many
-- The GitHub issues of one project that the workspace tracks, for a manual
-- sync of the project (multica issue sync --project), by repository and
-- number. Issues the reconcile found deleted or transferred away are left
-- out, and with open_only so are issues closed on GitHub. Read-only.
SELECT x.issue_id, x.external_repo, x.external_number, x.state
FROM issue_external x
JOIN issue i ON i.id = x.issue_id AND i.workspace_id = x.workspace_id
WHERE x.workspace_id = @workspace_id
  AND i.project_id = @project_id
  AND x.provider = 'github'
  AND (x.state_reason IS NULL OR x.state_reason NOT LIKE 'multica:%')
  AND (NOT @open_only::boolean OR x.state = 'open')
ORDER BY lower(x.external_repo), x.external_number;
