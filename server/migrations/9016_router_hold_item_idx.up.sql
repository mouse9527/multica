-- Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).
-- One hold row per inbox item: a repeated write of the same item (a caller
-- retrying) records nothing new.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_router_hold_item
    ON router_hold (inbox_item_id);
