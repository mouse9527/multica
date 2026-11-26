-- Added by the mouse9527/multica fork: cross-workspace read-only access for
-- agents (M-307, GitHub #207).
--
-- Grants and their audit trail go with the tables. With the tables gone the
-- feature cannot match any grant, so every cross-workspace task-token request
-- is refused as upstream refuses it.
DROP TABLE IF EXISTS cross_workspace_read_audit;
DROP TABLE IF EXISTS cross_workspace_read_grant;
