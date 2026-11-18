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
	// DefaultReconcileLookback is how far back each pass re-reads edge rows
	// once it has caught up. A row's time is set when it is inserted, not when
	// it commits, so a slow transaction can surface a row behind the cursor;
	// event_key keeps the re-read idempotent.
	DefaultReconcileLookback = 10 * time.Minute
	// DefaultReconcileBatch caps the rows one edge query reads. Each batch
	// commits on its own and moves that kind's cursor, so a pass cut short by
	// its budget keeps its progress and the next pass continues from there.
	// The lookback re-read runs only after the cursor has caught up and never
	// moves it, so it cannot hold progress back.
	DefaultReconcileBatch = 500
	// DefaultReconcileWorkspaceBudget bounds one workspace's share of a pass,
	// so a large backlog in one workspace cannot starve the others.
	DefaultReconcileWorkspaceBudget = time.Minute
)

// Reconciler adds the events the listener missed. Comments, terminal task
// failures and new issues are edges with stable keys, read in batches by
// time. Status and assignee are levels and are compared with
// router_issue_seen. Every row it adds is a capture_miss.
type Reconciler struct {
	Store           *Store
	Grace           time.Duration
	Lookback        time.Duration
	Batch           int
	WorkspaceBudget time.Duration
	Logger          *slog.Logger
}

func NewReconciler(store *Store) *Reconciler {
	return &Reconciler{
		Store: store, Grace: DefaultReconcileGrace, Lookback: DefaultReconcileLookback,
		Batch: DefaultReconcileBatch, WorkspaceBudget: DefaultReconcileWorkspaceBudget,
		Logger: slog.Default(),
	}
}

// ReconcileResult reports one pass.
type ReconcileResult struct {
	Workspaces int
	// Missed counts events added by this pass, by kind, including events
	// added before a workspace ran out of budget.
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
	upper, err := r.upperBound(ctx)
	if err != nil {
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
			r.Logger.Warn("router: reconcile did not finish; its progress is kept", "workspace_id", ws, "error", err)
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
	upper, err := r.upperBound(ctx)
	if err != nil {
		return res, err
	}
	return res, r.reconcileWorkspace(ctx, workspaceID, upper, res.Missed)
}

func (r *Reconciler) upperBound(ctx context.Context) (time.Time, error) {
	var upper time.Time
	err := r.Store.DB.QueryRow(ctx, `SELECT now() - make_interval(secs => $1)`, r.Grace.Seconds()).Scan(&upper)
	return upper, err
}

// edgeKind describes how reconcile reads one kind of edge event. The SQL
// fragments are constants; nothing here comes from input.
type edgeKind struct {
	kind  string
	join  string // joins the source table to members m
	where string // extra source filters
	ts    string // the event time the window and keyset order by
	item  string // the source row id, breaking ties within ts
	// cols yields event_key, kind, issue_id, actor_type, actor_id, is_self,
	// payload, occurred_at.
	cols string
}

var edgeKinds = []edgeKind{
	{
		kind: KindIssueCreated,
		join: "JOIN issue i ON i.id = m.issue_id", ts: "i.created_at", item: "i.id",
		cols: `'issue:' || i.id::text || ':created' AS event_key, 'issue_created'::text AS kind,
		       i.id AS issue_id, i.creator_type AS actor_type, i.creator_id AS actor_id,
		       (i.creator_type = 'agent' AND i.creator_id = m.agent_id) AS is_self,
		       '{}'::jsonb AS payload, i.created_at AS occurred_at`,
	},
	{
		kind: KindCommentCreated,
		join: "JOIN comment c ON c.issue_id = m.issue_id", where: " AND c.deleted_at IS NULL",
		ts: "c.created_at", item: "c.id",
		cols: `'comment:' || c.id::text AS event_key, 'comment_created'::text AS kind,
		       c.issue_id AS issue_id, c.author_type AS actor_type, c.author_id AS actor_id,
		       (c.author_type = 'agent' AND c.author_id = m.agent_id) AS is_self,
		       jsonb_strip_nulls(jsonb_build_object('comment_id', c.id::text, 'parent_id', c.parent_id::text)) AS payload,
		       c.created_at AS occurred_at`,
	},
	{
		// Terminal failures only: a failed task that was retried is not the
		// end of the run, matching the listener's retry_pending check.
		kind: KindTaskFailed,
		join: "JOIN agent_task_queue t ON t.issue_id = m.issue_id",
		where: ` AND t.status = 'failed'
		      AND NOT EXISTS (
		          SELECT 1 FROM agent_task_queue r
		          WHERE r.issue_id = t.issue_id AND r.retry_of_task_id = t.id)`,
		ts: "t.completed_at", item: "t.id",
		cols: `'task:' || t.id::text || ':failed' AS event_key, 'task_failed'::text AS kind,
		       t.issue_id AS issue_id, 'agent'::text AS actor_type, t.agent_id AS actor_id,
		       t.agent_id = m.agent_id AS is_self,
		       jsonb_strip_nulls(jsonb_build_object('task_id', t.id::text, 'failure_reason', t.failure_reason)) AS payload,
		       t.completed_at AS occurred_at`,
	},
}

// batchSQL reads the next batch of one edge kind in (time, row, subscription)
// order, inserts what each agent does not have yet, and reports the batch
// size, the inserted count and the batch's last key. The batch starts after
// the key ($3 time, $4 row id, $5 subscription id), or at $3 inclusive when
// the row id is NULL, and ends before $2. Rows from before a subscription
// started capturing are never read for it.
func (k edgeKind) batchSQL() string {
	return `WITH RECURSIVE ` + membersCTE + `,
batch AS (
    SELECT m.sub_id, m.agent_id, ` + k.cols + `,
           ` + k.ts + ` AS ts, ` + k.item + ` AS item
    FROM members m
    JOIN router_subscription s ON s.id = m.sub_id
    ` + k.join + `
    WHERE '` + k.kind + `' = ANY(s.event_types)
      AND ` + k.ts + ` >= s.active_since AND ` + k.ts + ` < $2` + k.where + `
      AND ($3::timestamptz IS NULL
           OR ($4::uuid IS NULL AND ` + k.ts + ` >= $3::timestamptz)
           OR ($4::uuid IS NOT NULL AND (` + k.ts + `, ` + k.item + `, m.sub_id) > ($3::timestamptz, $4::uuid, $5::uuid)))
    ORDER BY ts, item, m.sub_id
    LIMIT $6
),
ins AS (
    INSERT INTO router_event (
        workspace_id, subscription_id, agent_id, event_key, kind, issue_id,
        actor_type, actor_id, source, is_self, payload, occurred_at
    )
    SELECT $1::uuid, sub_id, agent_id, event_key, kind, issue_id,
           actor_type, actor_id, 'reconcile', is_self, payload, occurred_at
    FROM batch
    ON CONFLICT (agent_id, event_key) DO NOTHING
    RETURNING 1
),
last AS (
    SELECT ts, item, sub_id FROM batch ORDER BY ts DESC, item DESC, sub_id DESC LIMIT 1
)
SELECT (SELECT count(*) FROM batch), (SELECT count(*) FROM ins),
       (SELECT ts FROM last), (SELECT item::text FROM last), (SELECT sub_id::text FROM last)`
}

// edgeKey is a position in an edge kind's (time, row, subscription) order.
// A nil item means "from ts on, inclusive".
type edgeKey struct {
	ts   *time.Time
	item *string
	sub  *string
}

func (r *Reconciler) reconcileWorkspace(ctx context.Context, workspaceID string, upper time.Time, missed map[string]int) error {
	if r.WorkspaceBudget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.WorkspaceBudget)
		defer cancel()
	}
	db := r.Store.DB
	if err := syncSeen(ctx, db, workspaceID); err != nil {
		return err
	}
	// Levels first: their work is bounded by the scope's size and committed
	// per issue, so an edge backlog cannot keep them from running.
	if err := r.reconcileLevels(ctx, workspaceID, upper, missed); err != nil {
		return err
	}
	// Every kind moves forward before any lookback runs, so one kind's
	// re-read cannot use up the budget another kind needs to make progress.
	for _, k := range edgeKinds {
		if err := r.forwardEdges(ctx, workspaceID, upper, k, missed); err != nil {
			return fmt.Errorf("reconcile %s: %w", k.kind, err)
		}
	}
	if r.Lookback > 0 {
		from := upper.Add(-r.Lookback)
		for _, k := range edgeKinds {
			if err := r.walkEdges(ctx, workspaceID, upper, k, edgeKey{ts: &from}, missed, nil); err != nil {
				return fmt.Errorf("re-read %s: %w", k.kind, err)
			}
		}
	}
	_, err := db.Exec(ctx, `
		UPDATE router_subscription SET reconciled_through = $2
		WHERE workspace_id = $1::uuid AND enabled AND active_since <= $2`, workspaceID, upper)
	return err
}

// forwardEdges reads one edge kind from the workspace's stored cursor (the
// start of capture when there is none) up to upper, batch by batch. After
// each batch every row up to the batch's last key has been read, so the
// cursor moves there; a short batch means the window is done and the cursor
// moves to upper. A pass cut short keeps every finished batch.
//
// Rows that commit late, behind the cursor, are picked up by the lookback
// re-read in reconcileWorkspace, which runs once every kind has caught up and
// never moves a cursor.
func (r *Reconciler) forwardEdges(ctx context.Context, workspaceID string, upper time.Time, k edgeKind, missed map[string]int) error {
	var start edgeKey
	err := r.Store.DB.QueryRow(ctx, `
		SELECT ts, item_id::text, subscription_id::text FROM router_reconcile_cursor
		WHERE workspace_id = $1::uuid AND kind = $2`, workspaceID, k.kind,
	).Scan(&start.ts, &start.item, &start.sub)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	save := func(key edgeKey) error {
		_, err := r.Store.DB.Exec(ctx, `
			INSERT INTO router_reconcile_cursor (workspace_id, kind, ts, item_id, subscription_id)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid)
			ON CONFLICT (workspace_id, kind) DO UPDATE
			SET ts = EXCLUDED.ts, item_id = EXCLUDED.item_id,
			    subscription_id = EXCLUDED.subscription_id, updated_at = now()`,
			workspaceID, k.kind, key.ts, key.item, key.sub)
		return err
	}
	return r.walkEdges(ctx, workspaceID, upper, k, start, missed, save)
}

// walkEdges reads batches from key to upper, calling save (when set) with
// the position reached after each batch.
func (r *Reconciler) walkEdges(ctx context.Context, workspaceID string, upper time.Time, k edgeKind, key edgeKey, missed map[string]int, save func(edgeKey) error) error {
	batch := r.Batch
	if batch <= 0 {
		batch = DefaultReconcileBatch
	}
	query := k.batchSQL()
	for {
		var read, inserted int
		var last edgeKey
		if err := r.Store.DB.QueryRow(ctx, query, workspaceID, upper,
			key.ts, key.item, key.sub, batch,
		).Scan(&read, &inserted, &last.ts, &last.item, &last.sub); err != nil {
			return err
		}
		missed[k.kind] += inserted
		if read < batch {
			if save == nil {
				return nil
			}
			return save(edgeKey{ts: &upper})
		}
		if save != nil {
			if err := save(last); err != nil {
				return err
			}
		}
		key = last
	}
}

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
	// Changes made at or after upper are left for the next pass. The
	// listener never saw this change, so its actor is unknown.
	changes, err := recordLevel(ctx, tx, workspaceID, sub, issueID, levelInput{update: true, before: upper})
	if err != nil {
		return err
	}
	added := map[string]int{}
	if err := insertEventsCounting(ctx, tx, workspaceID, sub, changes, added); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for k, n := range added {
		missed[k] += n
	}
	return nil
}

func insertEventsCounting(ctx context.Context, tx pgx.Tx, workspaceID string, sub scopeSub, evs []pendingEvent, added map[string]int) error {
	for _, ev := range evs {
		n, err := insertEvents(ctx, tx, workspaceID, sub, []pendingEvent{ev}, SourceReconcile)
		if err != nil {
			return err
		}
		added[ev.Kind] += n
	}
	return nil
}
