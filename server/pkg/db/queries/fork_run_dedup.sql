-- Added by the mouse9527/multica fork: one run per operation (M-280).
-- The rules these queries encode, and why the windows are safe, are in
-- server/internal/service/fork_run_dedup.go.

-- name: ForkRebindQueuedRunsToStep :many
-- A workflow handoff reached an agent whose run on the issue is still queued
-- for an earlier step. Bind that run to the step the issue is entering, so its
-- claim renders this step's brief and its status changes are not refused as a
-- moved step. Nothing has been offered to a daemon yet, so nothing is lost.
UPDATE agent_task_queue t
SET workflow_step = sqlc.arg(status)::text
WHERE t.issue_id = sqlc.arg(issue_id)
  AND t.agent_id = sqlc.arg(agent_id)
  AND t.status = 'queued'
  AND t.chat_session_id IS NULL
  AND t.workflow_step IS DISTINCT FROM sqlc.arg(status)::text
  AND t.trigger_evidence_kind IN ('issue_assignment', 'comment')
  AND t.squad_id IS NULL
  AND NOT t.is_leader_task
  AND COALESCE(t.context->>'wakeup_id', '') = ''
RETURNING t.id;

-- name: ForkListSupersededStepRuns :many
-- The agent's runs on the issue that were claimed within the grace window and
-- are bound to a step the issue has just left. Claimed that recently, the run
-- has not read the issue yet; left alone it would work under the previous
-- step's brief and have its status changes refused.
SELECT t.id FROM agent_task_queue t
WHERE t.issue_id = sqlc.arg(issue_id)
  AND t.agent_id = sqlc.arg(agent_id)
  AND t.status IN ('dispatched', 'running')
  AND t.dispatched_at >= now() - sqlc.arg(grace_seconds)::float8 * interval '1 second'
  AND t.workflow_step IS DISTINCT FROM sqlc.arg(status)::text
  AND t.chat_session_id IS NULL
  AND t.trigger_evidence_kind IN ('issue_assignment', 'comment')
  AND t.squad_id IS NULL
  AND NOT t.is_leader_task
  AND t.rerun_of_task_id IS NULL
  AND t.retry_of_task_id IS NULL
  AND COALESCE(t.context->>'wakeup_id', '') = ''
ORDER BY t.dispatched_at;

-- name: ForkFoldCoveredQueuedRuns :many
-- Cancels, as folded into the run that just completed, every queued run of the
-- same agent on the same issue whose every input that run could read:
--
--   * an assignment arrived between lead_seconds before the completed run was
--     created and grace_seconds after it was claimed;
--   * every comment input arrived in that same window, and one that arrived
--     after the claim counts only when the claim did not tell the run to skip
--     its comment scan (the daemon claim's NewCommentsDeltaKnown with a zero
--     count; that is only possible for a comment-triggered run resuming an
--     earlier one with no new comment since it started);
--   * no status, assignee, title or description change landed between the
--     completed run's creation and the end of the window, so its claim saw
--     the step and issue the folded run would have seen.
--
-- The input checks run in the UPDATE itself, so a comment merged into the
-- queued run concurrently is re-checked against the row it lands on.
WITH done AS (
    SELECT t.id, t.issue_id, t.agent_id, t.squad_id, t.is_leader_task,
           t.trigger_comment_id, t.created_at, t.dispatched_at,
           t.context->>'head_sha' AS head_sha,
           t.created_at - sqlc.arg(lead_seconds)::float8 * interval '1 second' AS window_start,
           t.dispatched_at + sqlc.arg(grace_seconds)::float8 * interval '1 second' AS window_end
    FROM agent_task_queue t
    WHERE t.id = sqlc.arg(completed_task_id)
      AND t.status = 'completed'
      AND t.issue_id IS NOT NULL
      AND t.dispatched_at IS NOT NULL
      AND t.chat_session_id IS NULL
      AND t.trigger_evidence_kind IN ('issue_assignment', 'comment')
),
prior AS (
    -- The newest earlier run of the pair that had started when this one was
    -- claimed. The claim's comment count is anchored at or before it.
    SELECT max(p.started_at) AS started_at
    FROM agent_task_queue p
    JOIN done ON p.issue_id = done.issue_id AND p.agent_id = done.agent_id
    WHERE p.id <> done.id
      AND p.started_at < done.dispatched_at
),
covering AS (
    SELECT done.id, done.issue_id, done.agent_id, done.squad_id, done.is_leader_task,
           done.created_at, done.dispatched_at, done.head_sha,
           done.window_start, done.window_end,
           (done.trigger_comment_id IS NULL
            OR prior.started_at IS NULL
            OR EXISTS (
                SELECT 1 FROM comment c
                WHERE c.issue_id = done.issue_id
                  AND c.deleted_at IS NULL
                  AND c.created_at > prior.started_at
                  AND c.created_at <= done.dispatched_at
                  AND c.id <> done.trigger_comment_id
                  AND NOT (c.author_type = 'agent' AND c.author_id = done.agent_id)
            ))::boolean AS reads_comments
    FROM done CROSS JOIN prior
)
UPDATE agent_task_queue b
SET status = 'cancelled',
    completed_at = now(),
    prepare_lease_expires_at = NULL,
    error = 'Folded into run ' || covering.id::text || ', which could read every input of this run.',
    failure_reason = sqlc.arg(failure_reason)::text,
    cancelled_by_type = 'system',
    cancelled_by_id = NULL,
    cancelled_by_name = NULL
FROM covering
WHERE b.issue_id = covering.issue_id
  AND b.agent_id = covering.agent_id
  AND b.id <> covering.id
  AND b.status = 'queued'
  AND b.chat_session_id IS NULL
  AND b.trigger_evidence_kind IN ('issue_assignment', 'comment')
  AND b.rerun_of_task_id IS NULL
  AND b.retry_of_task_id IS NULL
  AND b.parent_task_id IS NULL
  AND b.autopilot_run_id IS NULL
  AND b.escalation_for_task_id IS NULL
  AND NOT b.force_fresh_session
  AND b.handoff_note IS NULL
  AND COALESCE(b.context->>'wakeup_id', '') = ''
  AND COALESCE(b.context->>'fresh_rerun', '') <> 'true'
  AND b.squad_id IS NOT DISTINCT FROM covering.squad_id
  AND b.is_leader_task = covering.is_leader_task
  AND (b.context->>'head_sha') IS NOT DISTINCT FROM covering.head_sha
  AND (b.trigger_comment_id IS NOT NULL
       OR b.created_at BETWEEN covering.window_start AND covering.window_end)
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(array_append(b.coalesced_comment_ids, b.trigger_comment_id)) AS input(id)
      LEFT JOIN comment c ON c.id = input.id
      WHERE input.id IS NOT NULL
        AND (c.id IS NULL
             OR c.created_at < covering.window_start
             OR c.created_at > covering.window_end
             OR (c.created_at > covering.dispatched_at AND NOT covering.reads_comments))
  )
  AND NOT EXISTS (
      SELECT 1 FROM activity_log al
      WHERE al.issue_id = covering.issue_id
        AND al.action IN ('status_changed', 'assignee_changed', 'title_changed', 'description_updated')
        AND al.created_at > covering.created_at
        AND al.created_at <= covering.window_end
  )
RETURNING b.*;
