-- Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).
-- The per-request lookup: the live grants a target workspace gave a source.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cross_workspace_read_grant_lookup
    ON cross_workspace_read_grant (target_workspace_id, source_workspace_id)
    WHERE revoked_at IS NULL;
