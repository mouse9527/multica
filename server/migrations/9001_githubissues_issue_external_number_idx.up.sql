-- Modified for the mouse9527/multica fork: GitHub-native issues.
-- One local issue per GitHub issue. Poll upserts and GitHub-side creates key on
-- this, so an echo or a re-poll never inserts a second issue.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_external_number
    ON issue_external (workspace_id, provider, external_repo_id, external_number);
