-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
-- The hourly run limit counts one subscription's recent dispatches.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_router_dispatch_subscription
    ON router_dispatch (subscription_id, created_at);
