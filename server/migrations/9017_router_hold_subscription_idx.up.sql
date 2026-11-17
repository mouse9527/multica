-- Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).
-- Dispatch lists a subscription's hold rows not yet shown in a batch, and
-- the API lists a subscription's rows newest first.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_router_hold_subscription
    ON router_hold (subscription_id, created_at);
