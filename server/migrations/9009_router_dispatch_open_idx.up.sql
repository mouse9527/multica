-- Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).
-- Dispatch finds an agent's open runs and settles open rows.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_router_dispatch_open
    ON router_dispatch (agent_id) WHERE settled_at IS NULL;
