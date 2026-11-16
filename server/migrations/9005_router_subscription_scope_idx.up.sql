-- Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).
-- One subscription per agent and scope.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_router_subscription_scope
    ON router_subscription (agent_id, scope_type, scope_id);
