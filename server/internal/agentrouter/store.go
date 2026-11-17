// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86), Router v6 slices 1 (M-96) and 2 (M-97), and Router page settings (M-105).

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
	// ErrStateConflict: the state changed since the revision the writer read.
	ErrStateConflict = errors.New("state revision does not match; read the state again and retry")
	// ErrStateTooLarge: the state exceeds MaxStateBytes.
	ErrStateTooLarge = fmt.Errorf("state exceeds %d bytes", MaxStateBytes)
	// ErrInvalid wraps a request that fails validation against stored values.
	ErrInvalid = errors.New("invalid subscription change")
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
    s.enabled, s.paused_reason, s.debounce_seconds, s.max_wait_seconds, s.max_runs_per_hour,
    s.state_revision, octet_length(s.state), s.active_since, s.reconciled_through,
    s.created_by_type, s.created_by_id::text, created.via_agent_id::text, created.via_task_id::text,
    s.created_at, s.updated_at,
    (SELECT ca.chat_session_id::text FROM router_chat_anchor ca WHERE ca.subscription_id = s.id),
    (SELECT count(*) FROM router_event e WHERE e.subscription_id = s.id),
    (SELECT count(*) FROM router_event e WHERE e.subscription_id = s.id AND e.source = 'reconcile')`

// subscriptionFrom joins each subscription with its create record, which
// carries the delegation (via) of an agent-created subscription.
const subscriptionFrom = `
    router_subscription s
    LEFT JOIN LATERAL (
        SELECT c.via_agent_id, c.via_task_id FROM router_subscription_change c
        WHERE c.subscription_id = s.id AND c.action = 'create'
        ORDER BY c.created_at LIMIT 1
    ) created ON true`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.AgentID, &s.OwnerUserID,
		&s.ScopeType, &s.ScopeID, &s.AnchorIssueID, &s.EventTypes,
		&s.Enabled, &s.PausedReason, &s.DebounceSeconds, &s.MaxWaitSeconds, &s.MaxRunsPerHour,
		&s.StateRevision, &s.StateBytes, &s.ActiveSince, &s.ReconciledThrough,
		&s.CreatedByType, &s.CreatedByID, &s.ViaAgentID, &s.ViaTaskID,
		&s.CreatedAt, &s.UpdatedAt, &s.AnchorChatSessionID,
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
	Batching      Batching
	CreatedByType string
	CreatedByID   string
	// AnchorChatSessionID, when set, binds the new subscription to a chat in
	// the same transaction (M-105), so it never dispatches an anchor run
	// between being created and being bound.
	AnchorChatSessionID string
	// Change records who created it and on whose authority; its Action is
	// set to "create".
	Change Change
}

// Change is one audit record of a subscription change.
type Change struct {
	Action    string
	ActorType string
	ActorID   string
	// AuthorizedBy is the member whose rights allowed the change.
	AuthorizedBy string
	// ViaAgentID and ViaTaskID name the agent run that acted for another
	// agent on AuthorizedBy's authority.
	ViaAgentID string
	ViaTaskID  string
	Detail     map[string]any
}

const (
	ChangeCreate    = "create"
	ChangeUpdate    = "update"
	ChangePause     = "pause"
	ChangeResume    = "resume"
	ChangeDelete    = "delete"
	ChangeAutoPause = "auto_pause"
)

// recordChange appends one audit row.
func recordChange(ctx context.Context, q Querier, workspaceID, subscriptionID string, c Change) error {
	detail, err := json.Marshal(c.Detail)
	if err != nil || c.Detail == nil {
		detail = []byte("{}")
	}
	_, err = q.Exec(ctx, `
		INSERT INTO router_subscription_change (
		    workspace_id, subscription_id, action, actor_type, actor_id,
		    authorized_by_user_id, via_agent_id, via_task_id, detail
		) VALUES ($1::uuid, $2::uuid, $3, $4, NULLIF($5, '')::uuid,
		          NULLIF($6, '')::uuid, NULLIF($7, '')::uuid, NULLIF($8, '')::uuid, $9)`,
		workspaceID, subscriptionID, c.Action, c.ActorType, c.ActorID,
		c.AuthorizedBy, c.ViaAgentID, c.ViaTaskID, detail)
	if err != nil {
		return fmt.Errorf("record subscription change: %w", err)
	}
	return nil
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

	if p.AnchorIssueID == "" && p.ScopeType == ScopeIssueTree {
		// Anchor runs go to the tree's root unless told otherwise.
		p.AnchorIssueID = p.ScopeID
	}
	b := p.Batching.withDefaults()
	if err := b.Validate(); err != nil {
		return Subscription{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO router_subscription (
		    workspace_id, agent_id, owner_user_id, scope_type, scope_id,
		    anchor_issue_id, event_types, created_by_type, created_by_id,
		    debounce_seconds, max_wait_seconds, max_runs_per_hour
		) VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, $4, $5::uuid,
		          NULLIF($6, '')::uuid, $7, $8, $9::uuid, $10, $11, $12)
		RETURNING id::text`,
		p.WorkspaceID, p.AgentID, p.OwnerUserID, p.ScopeType, p.ScopeID,
		p.AnchorIssueID, p.EventTypes, p.CreatedByType, p.CreatedByID,
		b.DebounceSeconds, b.MaxWaitSeconds, b.MaxRunsPerHour,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Subscription{}, ErrDuplicate
		}
		return Subscription{}, fmt.Errorf("insert subscription: %w", err)
	}
	change := p.Change
	change.Action = ChangeCreate
	if change.ActorType == "" {
		change.ActorType, change.ActorID = p.CreatedByType, p.CreatedByID
	}
	if p.AnchorChatSessionID != "" {
		bound := Subscription{ID: id, WorkspaceID: p.WorkspaceID, AgentID: p.AgentID}
		if err := setChatAnchor(ctx, tx, bound, p.AnchorIssueID, p.AnchorChatSessionID); err != nil {
			return Subscription{}, err
		}
		change.Detail = map[string]any{"anchor_chat_session_id": p.AnchorChatSessionID}
	}
	if err := recordChange(ctx, tx, p.WorkspaceID, id, change); err != nil {
		return Subscription{}, err
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
		FROM `+subscriptionFrom+`
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
		FROM `+subscriptionFrom+`
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
func (s *Store) Pause(ctx context.Context, workspaceID, id string, c Change) (Subscription, error) {
	c.Action = ChangePause
	return s.setEnabled(ctx, workspaceID, id, c, `
		UPDATE router_subscription
		SET enabled = false, paused_reason = 'manual', updated_at = now()
		WHERE workspace_id = $1::uuid AND id = $2::uuid AND enabled`)
}

// Resume restarts capture from now. A subscription whose dispatch was paused
// automatically (rate, agent_unavailable) kept capturing; resuming it only
// lets dispatch go on, with the events that arrived meanwhile.
func (s *Store) Resume(ctx context.Context, workspaceID, id string, c Change) (Subscription, error) {
	c.Action = ChangeResume
	return s.setEnabled(ctx, workspaceID, id, c, `
		UPDATE router_subscription
		SET enabled = true, paused_reason = NULL,
		    active_since = CASE WHEN enabled THEN active_since ELSE now() END,
		    reconciled_through = CASE WHEN enabled THEN reconciled_through END,
		    updated_at = now()
		WHERE workspace_id = $1::uuid AND id = $2::uuid
		  AND (NOT enabled OR paused_reason IS NOT NULL)`)
}

func (s *Store) setEnabled(ctx context.Context, workspaceID, id string, c Change, update string) (Subscription, error) {
	if _, err := s.Get(ctx, workspaceID, id); err != nil {
		return Subscription{}, err
	}
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return Subscription{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, update, workspaceID, id)
	if err != nil {
		return Subscription{}, err
	}
	if tag.RowsAffected() > 0 {
		if err := recordChange(ctx, tx, workspaceID, id, c); err != nil {
			return Subscription{}, err
		}
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

// Delete removes a subscription with its events, seen rows, dispatch records
// and chat binding. Its audit records stay.
func (s *Store) Delete(ctx context.Context, workspaceID, id string, c Change) error {
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
	if _, err := tx.Exec(ctx, `DELETE FROM router_dispatch WHERE subscription_id = $1::uuid`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM router_chat_anchor WHERE subscription_id = $1::uuid`, id); err != nil {
		return err
	}
	c.Action = ChangeDelete
	if err := recordChange(ctx, tx, workspaceID, id, c); err != nil {
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
	for _, table := range []string{
		"router_event", "router_issue_seen", "router_reconcile_cursor", "router_subscription",
		"router_dispatch", "router_subscription_change", "owner_notice", "router_chat_anchor",
		"router_hold",
	} {
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

// UpdateParams changes a subscription's dispatch settings. A nil or zero
// field keeps its value.
type UpdateParams struct {
	EventTypes    []string
	AnchorIssueID *string
	// AnchorChatSessionID binds the subscription to a chat session; an empty
	// string removes the binding. See setChatAnchor for what it must satisfy.
	AnchorChatSessionID *string
	Batching            Batching
	Change              Change
}

// Update applies p. Event types change what capture writes from now on;
// nothing already captured is removed.
func (s *Store) Update(ctx context.Context, workspaceID, id string, p UpdateParams) (Subscription, error) {
	cur, err := s.Get(ctx, workspaceID, id)
	if err != nil {
		return Subscription{}, err
	}
	b := Batching{DebounceSeconds: cur.DebounceSeconds, MaxWaitSeconds: cur.MaxWaitSeconds, MaxRunsPerHour: cur.MaxRunsPerHour}
	detail := map[string]any{}
	if p.Batching.DebounceSeconds != 0 {
		b.DebounceSeconds = p.Batching.DebounceSeconds
		detail["debounce_seconds"] = b.DebounceSeconds
	}
	if p.Batching.MaxWaitSeconds != 0 {
		b.MaxWaitSeconds = p.Batching.MaxWaitSeconds
		detail["max_wait_seconds"] = b.MaxWaitSeconds
	}
	if p.Batching.MaxRunsPerHour != 0 {
		b.MaxRunsPerHour = p.Batching.MaxRunsPerHour
		detail["max_runs_per_hour"] = b.MaxRunsPerHour
	}
	if err := b.Validate(); err != nil {
		return Subscription{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	eventTypes := cur.EventTypes
	if p.EventTypes != nil {
		eventTypes = p.EventTypes
		detail["event_types"] = eventTypes
	}
	anchor := ""
	if cur.AnchorIssueID != nil {
		anchor = *cur.AnchorIssueID
	}
	if p.AnchorIssueID != nil {
		anchor = *p.AnchorIssueID
		detail["anchor_issue_id"] = anchor
	}
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return Subscription{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `
		UPDATE router_subscription
		SET event_types = $3, anchor_issue_id = NULLIF($4, '')::uuid,
		    debounce_seconds = $5, max_wait_seconds = $6, max_runs_per_hour = $7,
		    updated_at = now()
		WHERE workspace_id = $1::uuid AND id = $2::uuid`,
		workspaceID, id, eventTypes, anchor, b.DebounceSeconds, b.MaxWaitSeconds, b.MaxRunsPerHour)
	if err != nil {
		return Subscription{}, err
	}
	if tag.RowsAffected() == 0 {
		return Subscription{}, ErrNotFound
	}
	if p.AnchorChatSessionID != nil {
		if err := setChatAnchor(ctx, tx, cur, anchor, *p.AnchorChatSessionID); err != nil {
			return Subscription{}, err
		}
		detail["anchor_chat_session_id"] = *p.AnchorChatSessionID
	}
	c := p.Change
	c.Action, c.Detail = ChangeUpdate, detail
	if err := recordChange(ctx, tx, workspaceID, id, c); err != nil {
		return Subscription{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Subscription{}, err
	}
	subscriptionGeneration.Add(1)
	return s.Get(ctx, workspaceID, id)
}

// State is a subscription's state document and its revision.
type State struct {
	SubscriptionID string    `json:"subscription_id"`
	State          string    `json:"state"`
	Revision       int64     `json:"revision"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// GetState returns a subscription's state.
func (s *Store) GetState(ctx context.Context, workspaceID, id string) (State, error) {
	st := State{SubscriptionID: id}
	err := s.DB.QueryRow(ctx, `
		SELECT state, state_revision, updated_at FROM router_subscription
		WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspaceID, id,
	).Scan(&st.State, &st.Revision, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrNotFound
	}
	return st, err
}

// SetState replaces the state when expectRevision is the current revision,
// and bumps the revision. A stale revision returns ErrStateConflict; the
// writer reads again and merges.
func (s *Store) SetState(ctx context.Context, workspaceID, id, state string, expectRevision int64) (State, error) {
	if len(state) > MaxStateBytes {
		return State{}, ErrStateTooLarge
	}
	st := State{SubscriptionID: id, State: state}
	err := s.DB.QueryRow(ctx, `
		UPDATE router_subscription
		SET state = $3, state_revision = state_revision + 1, updated_at = now()
		WHERE workspace_id = $1::uuid AND id = $2::uuid AND state_revision = $4
		RETURNING state_revision, updated_at`, workspaceID, id, state, expectRevision,
	).Scan(&st.Revision, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.GetState(ctx, workspaceID, id); err != nil {
			return State{}, err
		}
		return State{}, ErrStateConflict
	}
	return st, err
}

// ChangeRecord is one audit row as read back.
type ChangeRecord struct {
	ID                 string         `json:"id"`
	SubscriptionID     string         `json:"subscription_id"`
	Action             string         `json:"action"`
	ActorType          string         `json:"actor_type"`
	ActorID            *string        `json:"actor_id"`
	AuthorizedByUserID *string        `json:"authorized_by_user_id"`
	ViaAgentID         *string        `json:"via_agent_id"`
	ViaTaskID          *string        `json:"via_task_id"`
	Detail             map[string]any `json:"detail"`
	CreatedAt          time.Time      `json:"created_at"`
}

// ListChanges returns a subscription's audit trail, oldest first. It also
// works for a deleted subscription.
func (s *Store) ListChanges(ctx context.Context, workspaceID, id string) ([]ChangeRecord, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id::text, subscription_id::text, action, actor_type, actor_id::text,
		       authorized_by_user_id::text, via_agent_id::text, via_task_id::text, detail, created_at
		FROM router_subscription_change
		WHERE workspace_id = $1::uuid AND subscription_id = $2::uuid
		ORDER BY created_at, id`, workspaceID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChangeRecord{}
	for rows.Next() {
		var c ChangeRecord
		var detail []byte
		if err := rows.Scan(&c.ID, &c.SubscriptionID, &c.Action, &c.ActorType, &c.ActorID,
			&c.AuthorizedByUserID, &c.ViaAgentID, &c.ViaTaskID, &detail, &c.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &c.Detail)
		out = append(out, c)
	}
	return out, rows.Err()
}
