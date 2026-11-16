-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
-- The idempotency key of notify owner, scoped to one sender and one
-- recipient so nobody can read or suppress another sender's notices by
-- reusing a key. System notices have no sender_id.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_owner_notice_dedupe
    ON owner_notice (workspace_id, sender_type, (COALESCE(sender_id, '00000000-0000-0000-0000-000000000000'::uuid)), recipient_user_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;
