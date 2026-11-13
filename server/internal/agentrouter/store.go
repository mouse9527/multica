// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the slice of a pgx pool or transaction the store uses. All SQL
// here is hand-written: S1 adds no sqlc queries, so pkg/db/generated, which
// every upstream sync touches, stays as upstream has it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxStarter begins a transaction.
type TxStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

var (
	ErrNotFound  = errors.New("subscription not found")
	ErrDuplicate = errors.New("this agent already subscribes to this scope")
)

// subscriptionGeneration is bumped on every subscription write so the
// listener's cache in this process reloads on its next event instead of
// waiting out its TTL. Other replicas still wait for the TTL; reconcile covers
// the gap.
var subscriptionGeneration atomic.Int64

// Store reads and writes the Router tables.
type Store struct {
	DB Querier
	Tx TxStarter
}

func NewStore(db Querier, tx TxStarter) *Store { return &Store{DB: db, Tx: tx} }

// membersCTE resolves, for every enabled subscription in workspace $1, the
// issues it owns after the narrowest-scope rule (see pickNarrowest). It
// defines members(sub_id, agent_id, issue_id).
const membersCTE = `
subs AS (
    SELECT id, agent_id, scope_type, scope_id, created_at
    FROM router_subscription
    WHERE workspace_id = $1::uuid AND enabled
),
tree AS (
    SELECT s.id AS sub_id, i.id AS issue_id, 0 AS depth
    FROM subs s
    JOIN issue i ON i.id = s.scope_id AND i.workspace_id = $1::uuid
    WHERE s.scope_type = 'issue_tree'
    UNION ALL
    SELECT t.sub_id, c.id, t.depth + 1
    FROM tree t
    JOIN issue c ON c.parent_issue_id = t.issue_id
    WHERE t.depth < 32
),
cand AS (
    SELECT sub_id, issue_id, depth AS rank FROM tree
    UNION ALL
    SELECT s.id, i.id, 1000
    FROM subs s
    JOIN issue i ON i.project_id = s.scope_id AND i.workspace_id = $1::uuid
    WHERE s.scope_type = 'project'
),
members AS (
    SELECT DISTINCT ON (s.agent_id, c.issue_id) c.sub_id, s.agent_id, c.issue_id
    FROM cand c
    JOIN subs s ON s.id = c.sub_id
    ORDER BY s.agent_id, c.issue_id, c.rank, s.created_at, s.id
)`

const subscriptionColumns = `
    s.id::text, s.workspace_id::text, s.agent_id::text, s.owner_user_id::text,
    s.scope_type, s.scope_id::text, s.anchor_issue_id::text, s.event_types,
    s.enabled, s.paused_reason, s.active_since, s.reconcile_cursor,
    s.created_by_type, s.created_by_id::text, s.created_at, s.updated_at,
    (SELECT count(*) FROM router_event e WHERE e.subscription_id = s.id),
    (SELECT count(*) FROM router_event e WHERE e.subscription_id = s.id AND e.source = 'reconcile')`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.AgentID, &s.OwnerUserID,
		&s.ScopeType, &s.ScopeID, &s.AnchorIssueID, &s.EventTypes,
		&s.Enabled, &s.PausedReason, &s.ActiveSince, &s.ReconcileCursor,
		&s.CreatedByType, &s.CreatedByID, &s.CreatedAt, &s.UpdatedAt,
		&s.EventCount, &s.CaptureMiss)
	return s, err
}

// CreateParams is a validated new subscription.
type CreateParams struct {
	WorkspaceID   string
	AgentID       string
	OwnerUserID   string
	ScopeType     string
	ScopeID       string
	AnchorIssueID string
	EventTypes    []string
	CreatedByType string
	CreatedByID   string
}

// Create inserts a subscription and records the current state of every issue
// it owns, so the first reconcile compares against a baseline instead of
// mistaking pre-existing state for a missed change.
func (s *Store) Create(ctx context.Context, p CreateParams) (Subscription, error) {
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return Subscription{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO router_subscription (
		    workspace_id, agent_id, owner_user_id, scope_type, scope_id,
		    anchor_issue_id, event_types, created_by_type, created_by_id
		) VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, $4, $5::uuid,
		          NULLIF($6, '')::uuid, $7, $8, $9::uuid)
		RETURNING id::text`,
		p.WorkspaceID, p.AgentID, p.OwnerUserID, p.ScopeType, p.ScopeID,
		p.AnchorIssueID, p.EventTypes, p.CreatedByType, p.CreatedByID,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Subscription{}, ErrDuplicate
		}
		return Subscription{}, fmt.Errorf("insert subscription: %w", err)
	}
	if err := syncSeen(ctx, tx, p.WorkspaceID); err != nil {
		return Subscription{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Subscription{}, err
	}
	subscriptionGeneration.Add(1)
	return s.Get(ctx, p.WorkspaceID, id)
}

// Get returns one subscription in the workspace.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (Subscription, error) {
	sub, err := scanSubscription(s.DB.QueryRow(ctx, `
		SELECT `+subscriptionColumns+`
		FROM router_subscription s
		WHERE s.workspace_id = $1::uuid AND s.id = $2::uuid`, workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	return sub, err
}

// List returns the workspace's subscriptions, optionally for one agent.
func (s *Store) List(ctx context.Context, workspaceID, agentID string) ([]Subscription, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT `+subscriptionColumns+`
		FROM router_subscription s
		WHERE s.workspace_id = $1::uuid
		  AND ($2 = '' OR s.agent_id = NULLIF($2, '')::uuid)
		ORDER BY s.created_at, s.id`, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Pause stops capture for a subscription. Its seen rows go, so a resume
// starts from a fresh baseline and nothing that happened while paused counts
// as a miss.
func (s *Store) Pause(ctx context.Context, workspaceID, id string) (Subscription, error) {
	return s.setEnabled(ctx, workspaceID, id, `
		UPDATE router_subscription
		SET enabled = false, paused_reason = 'manual', updated_at = now()
		WHERE workspace_id = $1::uuid AND id = $2::uuid`)
}

// Resume restarts capture from now.
func (s *Store) Resume(ctx context.Context, workspaceID, id string) (Subscription, error) {
	return s.setEnabled(ctx, workspaceID, id, `
		UPDATE router_subscription
		SET enabled = true, paused_reason = NULL, active_since = now(),
		    reconcile_cursor = NULL, updated_at = now()
		WHERE workspace_id = $1::uuid AND id = $2::uuid AND NOT enabled`)
}

func (s *Store) setEnabled(ctx context.Context, workspaceID, id, update string) (Subscription, error) {
	if _, err := s.Get(ctx, workspaceID, id); err != nil {
		return Subscription{}, err
	}
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return Subscription{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, update, workspaceID, id); err != nil {
		return Subscription{}, err
	}
	if err := syncSeen(ctx, tx, workspaceID); err != nil {
		return Subscription{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Subscription{}, err
	}
	subscriptionGeneration.Add(1)
	return s.Get(ctx, workspaceID, id)
}

// Delete removes a subscription with its events and seen rows.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `DELETE FROM router_subscription WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspaceID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM router_event WHERE subscription_id = $1::uuid`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM router_issue_seen WHERE subscription_id = $1::uuid`, id); err != nil {
		return err
	}
	if err := syncSeen(ctx, tx, workspaceID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	subscriptionGeneration.Add(1)
	return nil
}

// syncSeen makes router_issue_seen match current ownership in a workspace:
// rows for issues a subscription no longer owns (paused, deleted, moved out of
// scope, or taken over by a narrower subscription of the same agent) are
// dropped, and an owned issue without a row gets one from its current state.
// A new row is a baseline, never an event.
func syncSeen(ctx context.Context, q Querier, workspaceID string) error {
	_, err := q.Exec(ctx, `
		WITH RECURSIVE `+membersCTE+`,
		dropped AS (
		    DELETE FROM router_issue_seen seen
		    WHERE seen.workspace_id = $1::uuid
		      AND NOT EXISTS (
		          SELECT 1 FROM members m
		          WHERE m.sub_id = seen.subscription_id AND m.issue_id = seen.issue_id)
		)
		INSERT INTO router_issue_seen (subscription_id, issue_id, workspace_id, status, assignee_type, assignee_id)
		SELECT m.sub_id, i.id, i.workspace_id, i.status, i.assignee_type, i.assignee_id
		FROM members m
		JOIN issue i ON i.id = m.issue_id
		ON CONFLICT (subscription_id, issue_id) DO NOTHING`, workspaceID)
	if err != nil {
		return fmt.Errorf("sync router seen state: %w", err)
	}
	return nil
}

// EventFilter narrows ListEvents.
type EventFilter struct {
	WorkspaceID    string
	SubscriptionID string
	AgentID        string
	Since          *time.Time
	// State is "", "pending" (not yet consumed by a run) or "consumed".
	State string
	// Source is "", "bus" or "reconcile".
	Source string
	Limit  int
	// HiddenProjects are private projects the reader may not see; events on
	// their issues are left out.
	HiddenProjects []string
}

// ListEvents returns events oldest first. With Since it returns the first
// Limit events after it; without, the latest Limit.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	order := "DESC"
	if f.Since != nil {
		order = "ASC"
	}
	hidden := f.HiddenProjects
	if hidden == nil {
		hidden = []string{}
	}
	rows, err := s.DB.Query(ctx, `
		SELECT * FROM (
		    SELECT e.id::text, e.subscription_id::text, e.agent_id::text, e.event_key, e.kind,
		           e.issue_id::text, COALESCE(w.issue_prefix || '-' || i.number, ''),
		           e.actor_type, e.actor_id::text, e.source, e.is_self, e.payload,
		           e.occurred_at, e.consumed_task_id::text, e.created_at
		    FROM router_event e
		    LEFT JOIN issue i ON i.id = e.issue_id
		    LEFT JOIN workspace w ON w.id = i.workspace_id
		    WHERE e.workspace_id = $1::uuid
		      AND ($2 = '' OR e.subscription_id = NULLIF($2, '')::uuid)
		      AND ($3 = '' OR e.agent_id = NULLIF($3, '')::uuid)
		      AND ($4::timestamptz IS NULL OR e.created_at > $4::timestamptz)
		      AND ($5 = '' OR ($5 = 'pending' AND e.consumed_task_id IS NULL)
		                   OR ($5 = 'consumed' AND e.consumed_task_id IS NOT NULL))
		      AND ($6 = '' OR e.source = $6)
		      AND (i.project_id IS NULL OR NOT (i.project_id = ANY($7::uuid[])))
		    ORDER BY e.created_at `+order+`, e.id `+order+`
		    LIMIT $8
		) page
		ORDER BY 15, 1`,
		f.WorkspaceID, f.SubscriptionID, f.AgentID, f.Since, f.State, f.Source, hidden, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.SubscriptionID, &e.AgentID, &e.EventKey, &e.Kind,
			&e.IssueID, &e.IssueIdentifier, &e.ActorType, &e.ActorID, &e.Source, &e.Self,
			&payload, &e.OccurredAt, &e.ConsumedTaskID, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Scope returns the issues a subscription currently owns. includeClosed adds
// done and cancelled issues. A paused subscription owns nothing.
func (s *Store) Scope(ctx context.Context, workspaceID, id string, includeClosed bool, hiddenProjects []string) ([]ScopeIssue, error) {
	if hiddenProjects == nil {
		hiddenProjects = []string{}
	}
	rows, err := s.DB.Query(ctx, `
		WITH RECURSIVE `+membersCTE+`
		SELECT i.id::text, w.issue_prefix || '-' || i.number, i.title, i.status,
		       i.assignee_type, i.assignee_id::text, i.parent_issue_id::text, i.updated_at
		FROM members m
		JOIN issue i ON i.id = m.issue_id
		JOIN workspace w ON w.id = i.workspace_id
		WHERE m.sub_id = $2::uuid
		  AND ($3 OR i.status NOT IN ('done', 'cancelled'))
		  AND (i.project_id IS NULL OR NOT (i.project_id = ANY($4::uuid[])))
		ORDER BY i.number`, workspaceID, id, includeClosed, hiddenProjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScopeIssue{}
	for rows.Next() {
		var si ScopeIssue
		if err := rows.Scan(&si.ID, &si.Identifier, &si.Title, &si.Status,
			&si.AssigneeType, &si.AssigneeID, &si.ParentID, &si.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// DeleteWorkspaceData removes every Router row of a workspace. It runs inside
// the workspace delete transaction; the tables have no foreign keys.
func DeleteWorkspaceData(ctx context.Context, q Querier, workspaceID any) error {
	for _, table := range []string{"router_event", "router_issue_seen", "router_subscription"} {
		if _, err := q.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspaceID); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return nil
}

// validScopeType reports whether t is a scope type.
func validScopeType(t string) bool { return t == ScopeIssueTree || t == ScopeProject }

// ParseScopeType validates a scope type from a request.
func ParseScopeType(t string) (string, error) {
	t = strings.TrimSpace(t)
	if !validScopeType(t) {
		return "", fmt.Errorf("scope_type must be %s or %s", ScopeIssueTree, ScopeProject)
	}
	return t, nil
}
