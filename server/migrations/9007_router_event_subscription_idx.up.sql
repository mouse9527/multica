-- Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).
-- Event listing and capture_miss counts read one subscription's events.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_router_event_subscription
    ON router_event (subscription_id, created_at);
