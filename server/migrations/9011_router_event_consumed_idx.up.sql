-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
-- A failed run hands its batch back by consumed_task_id.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_router_event_consumed
    ON router_event (consumed_task_id) WHERE consumed_task_id IS NOT NULL;
