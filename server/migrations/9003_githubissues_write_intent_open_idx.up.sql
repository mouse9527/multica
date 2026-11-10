-- Modified for the mouse9527/multica fork: GitHub-native issues.
-- Reconciliation scans only intents that are not settled yet; applied and
-- failed rows, the bulk of the table, stay out of the index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_github_write_intent_open
    ON github_write_intent (workspace_id, created_at)
    WHERE state IN ('pending', 'github_ok', 'unknown');
