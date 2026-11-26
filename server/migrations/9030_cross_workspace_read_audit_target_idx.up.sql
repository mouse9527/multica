-- Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).
-- A target workspace's audit trail, newest first.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cross_workspace_read_audit_target
    ON cross_workspace_read_audit (target_workspace_id, created_at DESC);
