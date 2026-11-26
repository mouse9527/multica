-- name: HasTaskMessages :one
-- Cancellation only needs to know whether a transcript exists.
-- Model API retry rows (type 'error', tool 'api_retry') are progress, not
-- transcript output (mouse9527/multica fork, M-312).
SELECT EXISTS (SELECT 1 FROM task_message WHERE task_id = $1 AND (type <> 'error' OR tool IS DISTINCT FROM 'api_retry'));
