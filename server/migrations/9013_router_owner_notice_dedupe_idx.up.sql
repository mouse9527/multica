-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
-- The idempotency key of notify owner: one notice per (workspace, dedupe_key).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_owner_notice_dedupe
    ON owner_notice (workspace_id, dedupe_key) WHERE dedupe_key IS NOT NULL;
