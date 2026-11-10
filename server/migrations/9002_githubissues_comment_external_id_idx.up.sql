-- Modified for the mouse9527/multica fork: GitHub-native issues.
-- One local comment per GitHub comment in a workspace; the comment poller
-- resolves GitHub comment ids through it.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_comment_external_external_id
    ON comment_external (workspace_id, external_id);
