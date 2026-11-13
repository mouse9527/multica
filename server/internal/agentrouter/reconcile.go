// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// DefaultReconcileInterval is how often reconcile runs (Owner, 2026-11-13).
	DefaultReconcileInterval = 5 * time.Minute
	// DefaultReconcileGrace keeps reconcile away from changes so recent that
	// the listener may still be writing them. Without it a race with the
	// listener would be counted as a miss.
	DefaultReconcileGrace = 30 * time.Second
	// DefaultReconcileLookback re-reads this much before the previous pass's
	// upper bound. A row's created_at is set when it is inserted, not when it
	// commits, so a slow transaction can surface a row older than the last
	// cursor; event_key keeps the re-read idempotent.
	DefaultReconcileLookback = 10 * time.Minute
)

// Reconciler adds the events the listener missed. Comments, terminal task
// failures and new issues are edges with stable keys and are re-read by
// time window. Status and assignee are levels and are compared with
// router_issue_seen. Every row it adds is a capture_miss.
type Reconciler struct {
	Store    *Store
	Grace    time.Duration
	Lookback time.Duration
	Logger   *slog.Logger
}

func NewReconciler(store *Store) *Reconciler {
	return &Reconciler{Store: store, Grace: DefaultReconcileGrace, Lookback: DefaultReconcileLookback, Logger: slog.Default()}
}

// ReconcileResult reports one pass.
type ReconcileResult struct {
	Workspaces int
	// Missed counts events added by this pass, by kind.
	Missed map[string]int
}

// Total is the pass's capture_miss.
func (r ReconcileResult) Total() int {
	n := 0
	for _, v := range r.Missed {
		n += v
	}
	return n
}

// Run reconciles every workspace that has an enabled subscription. A failing
// workspace is logged and skipped; the error returned joins them all.
func (r *Reconciler) Run(ctx context.Context) (ReconcileResult, error) {
	res := ReconcileResult{Missed: map[string]int{}}
	var upper time.Time
	if err := r.Store.DB.QueryRow(ctx, `SELECT now() - make_interval(secs => $1)`, r.Grace.Seconds()).Scan(&upper); err != nil {
		return res, err
	}
	rows, err := r.Store.DB.Query(ctx, `SELECT DISTINCT workspace_id::text FROM router_subscription WHERE enabled`)
	if err != nil {
		return res, err
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return res, err
	}
	var errs []error
	for _, ws := range workspaces {
		if err := r.reconcileWorkspace(ctx, ws, upper, res.Missed); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", ws, err))
			r.Logger.Warn("router: reconcile failed", "workspace_id", ws, "error", err)
			continue
		}
		res.Workspaces++
	}
	if total := res.Total(); total > 0 {
		r.Logger.Info("router: reconcile added events the listener missed",
			"capture_miss", total, "by_kind", res.Missed)
	}
	return res, errors.Join(errs...)
}

// RunWorkspace reconciles one workspace, for tests and manual repair.
func (r *Reconciler) RunWorkspace(ctx context.Context, workspaceID string) (ReconcileResult, error) {
	res := ReconcileResult{Workspaces: 1, Missed: map[string]int{}}
	var upper time.Time
	if err := r.Store.DB.QueryRow(ctx, `SELECT now() - make_interval(secs => $1)`, r.Grace.Seconds()).Scan(&upper); err != nil {
		return res, err
	}
	return res, r.reconcileWorkspace(ctx, workspaceID, upper, res.Missed)
}

// windowCTE gives each enabled subscription in workspace $1 the lower bound
// of its edge window: the previous upper bound minus the lookback ($3
// seconds), but never before capture started.
const windowCTE = `
win AS (
    SELECT id, event_types,
           GREATEST(active_since, COALESCE(reconcile_cursor - make_interval(secs => $3), active_since)) AS lower
    FROM router_subscription
    WHERE workspace_id = $1::uuid AND enabled
)`

const reconcileInsert = `
INSERT INTO router_event (
    workspace_id, subscription_id, agent_id, event_key, kind, issue_id,
    actor_type, actor_id, source, is_self, payload, occurred_at
)`

var reconcileEdgeQueries = []string{
	// New issues in scope.
	`WITH RECURSIVE ` + membersCTE + `,` + windowCTE + reconcileInsert + `
	SELECT $1::uuid, m.sub_id, m.agent_id, 'issue:' || i.id::text || ':created', 'issue_created', i.id,
	       i.creator_type, i.creator_id, 'reconcile',
	       i.creator_type = 'agent' AND i.creator_id = m.agent_id, '{}'::jsonb, i.created_at
	FROM members m
	JOIN win ON win.id = m.sub_id
	JOIN issue i ON i.id = m.issue_id
	WHERE 'issue_created' = ANY(win.event_types)
	  AND i.created_at >= win.lower AND i.created_at < $2
	ON CONFLICT (agent_id, event_key) DO NOTHING
	RETURNING kind`,
	// Comments.
	`WITH RECURSIVE ` + membersCTE + `,` + windowCTE + reconcileInsert + `
	SELECT $1::uuid, m.sub_id, m.agent_id, 'comment:' || c.id::text, 'comment_created', c.issue_id,
	       c.author_type, c.author_id, 'reconcile',
	       c.author_type = 'agent' AND c.author_id = m.agent_id,
	       jsonb_strip_nulls(jsonb_build_object('comment_id', c.id::text, 'parent_id', c.parent_id::text)),
	       c.created_at
	FROM members m
	JOIN win ON win.id = m.sub_id
	JOIN comment c ON c.issue_id = m.issue_id
	WHERE 'comment_created' = ANY(win.event_types)
	  AND c.deleted_at IS NULL
	  AND c.created_at >= win.lower AND c.created_at < $2
	ON CONFLICT (agent_id, event_key) DO NOTHING
	RETURNING kind`,
	// Terminal task failures: a failed task that was retried is not the end
	// of the run, matching the listener's retry_pending check.
	`WITH RECURSIVE ` + membersCTE + `,` + windowCTE + reconcileInsert + `
	SELECT $1::uuid, m.sub_id, m.agent_id, 'task:' || t.id::text || ':failed', 'task_failed', t.issue_id,
	       'agent', t.agent_id, 'reconcile', t.agent_id = m.agent_id,
	       jsonb_strip_nulls(jsonb_build_object('task_id', t.id::text, 'failure_reason', t.failure_reason)),
	       t.completed_at
	FROM members m
	JOIN win ON win.id = m.sub_id
	JOIN agent_task_queue t ON t.issue_id = m.issue_id
	WHERE 'task_failed' = ANY(win.event_types)
	  AND t.status = 'failed'
	  AND t.completed_at >= win.lower AND t.completed_at < $2
	  AND NOT EXISTS (
	      SELECT 1 FROM agent_task_queue r
	      WHERE r.issue_id = t.issue_id AND r.retry_of_task_id = t.id)
	ON CONFLICT (agent_id, event_key) DO NOTHING
	RETURNING kind`,
}

func (r *Reconciler) reconcileWorkspace(ctx context.Context, workspaceID string, upper time.Time, missed map[string]int) error {
	db := r.Store.DB
	if err := syncSeen(ctx, db, workspaceID); err != nil {
		return err
	}

	for _, q := range reconcileEdgeQueries {
		rows, err := db.Query(ctx, q, workspaceID, upper, r.Lookback.Seconds())
		if err != nil {
			return fmt.Errorf("reconcile edge events: %w", err)
		}
		kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("reconcile edge events: %w", err)
		}
		for _, k := range kinds {
			missed[k]++
		}
	}

	if err := r.reconcileLevels(ctx, workspaceID, upper, missed); err != nil {
		return err
	}

	_, err := db.Exec(ctx, `
		UPDATE router_subscription SET reconcile_cursor = $2
		WHERE workspace_id = $1::uuid AND enabled`, workspaceID, upper)
	return err
}

// reconcileLevels emits status and assignee changes the seen table has not
// recorded. Each candidate is rechecked under the seen row's lock, the same
// lock the listener takes, so the two never both record one change.
func (r *Reconciler) reconcileLevels(ctx context.Context, workspaceID string, upper time.Time, missed map[string]int) error {
	rows, err := r.Store.DB.Query(ctx, `
		WITH RECURSIVE `+membersCTE+`
		SELECT m.sub_id::text, m.agent_id::text, i.id::text
		FROM members m
		JOIN issue i ON i.id = m.issue_id
		JOIN router_issue_seen seen ON seen.subscription_id = m.sub_id AND seen.issue_id = i.id
		WHERE i.updated_at < $2
		  AND (seen.status <> i.status
		       OR seen.assignee_type IS DISTINCT FROM i.assignee_type
		       OR seen.assignee_id IS DISTINCT FROM i.assignee_id)`, workspaceID, upper)
	if err != nil {
		return fmt.Errorf("find level changes: %w", err)
	}
	type candidate struct{ subID, agentID, issueID string }
	cands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.subID, &c.agentID, &c.issueID)
		return c, err
	})
	if err != nil {
		return fmt.Errorf("find level changes: %w", err)
	}
	for _, c := range cands {
		if err := r.reconcileLevel(ctx, workspaceID, upper, c.subID, c.issueID, missed); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileLevel(ctx context.Context, workspaceID string, upper time.Time, subID, issueID string, missed map[string]int) error {
	tx, err := r.Store.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var sub scopeSub
	err = tx.QueryRow(ctx, `
		SELECT id::text, agent_id::text, scope_type, scope_id::text, event_types, created_at
		FROM router_subscription WHERE id = $1::uuid AND enabled`, subID,
	).Scan(&sub.ID, &sub.AgentID, &sub.ScopeType, &sub.ScopeID, &sub.EventTypes, &sub.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	issue, err := loadIssue(ctx, tx, workspaceID, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !issue.UpdatedAt.Before(upper) {
		// Changed again since the candidate query; the next pass sees it.
		return nil
	}
	// The listener never saw this change, so its actor is unknown.
	changes, err := recordLevel(ctx, tx, workspaceID, sub, issue, true, nil, nil)
	if err != nil {
		return err
	}
	if _, err := insertEventsCounting(ctx, tx, workspaceID, sub, changes, missed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertEventsCounting(ctx context.Context, tx pgx.Tx, workspaceID string, sub scopeSub, evs []pendingEvent, missed map[string]int) (int, error) {
	total := 0
	for _, ev := range evs {
		n, err := insertEvents(ctx, tx, workspaceID, sub, []pendingEvent{ev}, SourceReconcile)
		if err != nil {
			return total, err
		}
		missed[ev.Kind] += n
		total += n
	}
	return total, nil
}
