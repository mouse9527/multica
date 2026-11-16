-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
-- A subscription's audit trail, newest first.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_router_subscription_change
    ON router_subscription_change (subscription_id, created_at);
