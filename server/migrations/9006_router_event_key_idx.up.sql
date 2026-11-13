-- Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).
-- The idempotency key: the listener and reconcile insert with ON CONFLICT DO
-- NOTHING on it.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_router_event_key
    ON router_event (agent_id, event_key);
