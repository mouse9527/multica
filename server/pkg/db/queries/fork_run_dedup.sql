-- Added by the mouse9527/multica fork: one run per operation (M-280).
-- The rules these queries encode are in server/internal/service/fork_run_dedup.go.

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
-- The agent's runs on the issue claimed within the grace window and bound to
-- a step the issue has just left. Left alone such a run works under the
-- previous step's brief and has its status changes refused.
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

-- name: ForkListRunsSupersededByComment :many
-- The agent's runs on the issue that were claimed within the grace window
-- before the comment was posted, and whose reply belongs where the comment's
-- would: a run outside any comment thread (an assignment or handoff), or a run
-- in the comment's own thread. Their claims could not include the comment.
SELECT t.id FROM agent_task_queue t
JOIN comment c ON c.id = sqlc.arg(comment_id)
WHERE t.issue_id = sqlc.arg(issue_id)
  AND t.agent_id = sqlc.arg(agent_id)
  AND c.issue_id = t.issue_id
  AND t.status IN ('dispatched', 'running')
  AND t.dispatched_at >= now() - sqlc.arg(grace_seconds)::float8 * interval '1 second'
  AND t.dispatched_at < c.created_at
  AND (t.comment_thread_id IS NULL OR t.comment_thread_id = comment_thread_root_id(c.id))
  AND t.trigger_comment_id IS DISTINCT FROM c.id
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
-- same agent on the same issue whose every input provably reached that run:
--
--   * a comment input is in the run's delivered_comment_ids receipt, or already
--     existed when the run was claimed (and at most lead_seconds before it was
--     created), so the claim counted it in the run's comment delta or left the
--     run to scan for it. A comment posted after the claim never counts;
--   * an assignment input (a queued run without a trigger comment) was created
--     in that same window, before the claim;
--   * no status, assignee, title or description change landed between the
--     run's creation and its claim, so the claim rendered the step and issue
--     the queued run was created under.
--
-- The checks run in the UPDATE itself, so a comment merged into the queued run
-- concurrently is re-checked against the row it lands on.
WITH done AS (
    SELECT t.id, t.issue_id, t.agent_id, t.squad_id, t.is_leader_task,
           t.created_at, t.dispatched_at,
           COALESCE(t.delivered_comment_ids, '{}'::uuid[]) AS delivered_comment_ids,
           t.context->>'head_sha' AS head_sha,
           t.created_at - sqlc.arg(lead_seconds)::float8 * interval '1 second' AS window_start
    FROM agent_task_queue t
    WHERE t.id = sqlc.arg(completed_task_id)
      AND t.status = 'completed'
      AND t.issue_id IS NOT NULL
      AND t.dispatched_at IS NOT NULL
      AND t.chat_session_id IS NULL
      AND t.trigger_evidence_kind IN ('issue_assignment', 'comment')
)
UPDATE agent_task_queue b
SET status = 'cancelled',
    completed_at = now(),
    prepare_lease_expires_at = NULL,
    error = 'Folded into run ' || done.id::text || ', which received every input of this run.',
    failure_reason = sqlc.arg(failure_reason)::text,
    cancelled_by_type = 'system',
    cancelled_by_id = NULL,
    cancelled_by_name = NULL
FROM done
WHERE b.issue_id = done.issue_id
  AND b.agent_id = done.agent_id
  AND b.id <> done.id
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
  AND b.squad_id IS NOT DISTINCT FROM done.squad_id
  AND b.is_leader_task = done.is_leader_task
  AND (b.context->>'head_sha') IS NOT DISTINCT FROM done.head_sha
  AND (b.trigger_comment_id IS NOT NULL
       OR b.created_at BETWEEN done.window_start AND done.dispatched_at)
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(array_append(b.coalesced_comment_ids, b.trigger_comment_id)) AS input(id)
      LEFT JOIN comment c ON c.id = input.id
      WHERE input.id IS NOT NULL
        AND NOT (input.id = ANY(done.delivered_comment_ids))
        AND (c.id IS NULL
             OR c.created_at < done.window_start
             OR c.created_at > done.dispatched_at)
  )
  AND NOT EXISTS (
      SELECT 1 FROM activity_log al
      WHERE al.issue_id = done.issue_id
        AND al.action IN ('status_changed', 'assignee_changed', 'title_changed', 'description_updated')
        AND al.created_at > done.created_at
        AND al.created_at <= done.dispatched_at
  )
RETURNING b.*;
