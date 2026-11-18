-- Modified for the mouse9527/multica fork: GitHub-native issues.
-- Tables come from migration 9000.
--
-- The feature was withdrawn on 2026-11-12 (M-70); no code reads or writes these
-- tables any more. They stay in the schema until a separate migration drops
-- them, so workspace teardown still clears their rows.

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
