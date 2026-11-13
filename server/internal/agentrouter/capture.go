// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	// DefaultCacheTTL is how long the listener trusts its copy of the enabled
	// subscriptions before reloading them.
	DefaultCacheTTL = 30 * time.Second
	// DefaultCaptureTimeout bounds the listener's database work per event.
	// The listener runs on the publisher's goroutine, usually a request, so a
	// slow capture slows the write that triggered it; past this budget the
	// capture is dropped and reconcile picks the change up instead.
	DefaultCaptureTimeout = 500 * time.Millisecond

	// Reload backoff after a failed subscription reload: 1s, doubling, at
	// most 30s, so a database outage costs one bounded query per interval
	// instead of one per published event.
	reloadBackoffBase = time.Second
	reloadBackoffMax  = 30 * time.Second
)

// Capturer is the events.Bus listener that writes router_event rows.
type Capturer struct {
	Store   *Store
	TTL     time.Duration
	Timeout time.Duration
	Logger  *slog.Logger

	// snap is the cached subscription set. Publishers read it without
	// locking; at most one publisher at a time reloads it (reloading), and
	// every other publisher keeps using the old snapshot meanwhile.
	snap      atomic.Pointer[subscriptionSnapshot]
	reloading atomic.Bool
	// reloadFailures and retryAt are touched only by the publisher holding
	// reloading, whose CompareAndSwap orders them between holders.
	reloadFailures int
	retryAt        time.Time

	// Failures counts captures that errored or timed out. Each one is a
	// change reconcile is expected to add later as a capture_miss.
	Failures atomic.Int64
}

func NewCapturer(store *Store) *Capturer {
	return &Capturer{Store: store, TTL: DefaultCacheTTL, Timeout: DefaultCaptureTimeout, Logger: slog.Default()}
}

// Subscribe registers the listener for the event types S1 captures.
func (c *Capturer) Subscribe(bus *events.Bus) {
	bus.Subscribe(protocol.EventIssueCreated, c.Handle)
	bus.Subscribe(protocol.EventIssueUpdated, c.Handle)
	bus.Subscribe(protocol.EventCommentCreated, c.Handle)
	bus.Subscribe(protocol.EventTaskFailed, c.Handle)
}

type subscriptionSnapshot struct {
	byWS     map[string][]scopeSub
	gen      int64
	loadedAt time.Time
}

func (s *subscriptionSnapshot) forWorkspace(workspaceID string) []scopeSub {
	if s == nil {
		return nil
	}
	return s.byWS[workspaceID]
}

// trigger is what an event tells the listener: which issue, and the comment or
// task behind it when there is one. Everything else is read from the database.
type trigger struct {
	eventType string
	issueID   string
	commentID string
	taskID    string
	actorType string
	actorID   string
	// issueState is the issue as the publisher saw it, for issue events.
	issueState *payloadIssue
}

// payloadIssue is the issue state an issue event carries.
type payloadIssue struct {
	Status       string  `json:"status"`
	AssigneeType *string `json:"assignee_type"`
	AssigneeID   *string `json:"assignee_id"`
	UpdatedAt    string  `json:"updated_at"`
}

// describes reports whether the event's issue state is the state now in the
// database, i.e. whether the change found under the seen lock is the one this
// event announced. Payload timestamps carry whole seconds only, so a second
// write within the same second with the same fields is indistinguishable;
// it would be credited to this event's actor.
func (p *payloadIssue) describes(cur issueRow) bool {
	if p == nil || p.Status != cur.Status ||
		!sameRef(p.AssigneeType, cur.AssigneeType) || !sameRef(p.AssigneeID, cur.AssigneeID) {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, p.UpdatedAt)
	return err == nil && at.Unix() == cur.UpdatedAt.Unix()
}

// Handle captures one event. It never panics into the publisher and never
// returns an error: a failure is logged and counted, and reconcile repairs it.
func (c *Capturer) Handle(e events.Event) {
	if e.WorkspaceID == "" {
		return
	}
	// One budget covers the cache reload and the capture, so the publisher
	// never waits longer than Timeout.
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	subs := c.subscriptions(ctx, e.WorkspaceID)
	if len(subs) == 0 {
		return
	}
	t, ok := parseTrigger(e)
	if !ok {
		return
	}
	if err := c.capture(ctx, e.WorkspaceID, subs, t); err != nil {
		n := c.Failures.Add(1)
		c.Logger.Warn("router: capture failed; reconcile will add the event",
			"event_type", e.Type, "workspace_id", e.WorkspaceID, "issue_id", t.issueID,
			"failures_total", n, "error", err)
	}
}

// subscriptions returns the workspace's enabled subscriptions from the
// cache. When the cache is stale, or a subscription changed in this process,
// the first publisher to notice reloads every subscription in one query; any
// publisher arriving during that reload uses the old snapshot (nothing, before
// the first load) instead of waiting. A failed reload keeps the old snapshot
// and is not retried until its backoff passes.
func (c *Capturer) subscriptions(ctx context.Context, workspaceID string) []scopeSub {
	snap := c.snap.Load()
	gen := subscriptionGeneration.Load()
	if snap != nil && snap.gen == gen && time.Since(snap.loadedAt) < c.TTL {
		return snap.forWorkspace(workspaceID)
	}
	if !c.reloading.CompareAndSwap(false, true) {
		return snap.forWorkspace(workspaceID)
	}
	defer c.reloading.Store(false)
	if time.Now().Before(c.retryAt) {
		return snap.forWorkspace(workspaceID)
	}
	byWS, err := c.loadSubscriptions(ctx)
	if err != nil {
		c.reloadFailures++
		backoff := reloadBackoffBase << min(c.reloadFailures-1, 5)
		c.retryAt = time.Now().Add(min(backoff, reloadBackoffMax))
		c.Logger.Warn("router: reload subscriptions failed; using the previous copy",
			"retry_in", min(backoff, reloadBackoffMax), "error", err)
		return snap.forWorkspace(workspaceID)
	}
	c.reloadFailures, c.retryAt = 0, time.Time{}
	next := &subscriptionSnapshot{byWS: byWS, gen: gen, loadedAt: time.Now()}
	c.snap.Store(next)
	return next.forWorkspace(workspaceID)
}

func (c *Capturer) loadSubscriptions(ctx context.Context) (map[string][]scopeSub, error) {
	rows, err := c.Store.DB.Query(ctx, `
		SELECT id::text, workspace_id::text, agent_id::text, scope_type, scope_id::text, event_types, created_at
		FROM router_subscription
		WHERE enabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]scopeSub{}
	for rows.Next() {
		var s scopeSub
		var ws string
		if err := rows.Scan(&s.ID, &ws, &s.AgentID, &s.ScopeType, &s.ScopeID, &s.EventTypes, &s.CreatedAt); err != nil {
			return nil, err
		}
		out[ws] = append(out[ws], s)
	}
	return out, rows.Err()
}

// parseTrigger pulls ids out of the event payload. Payloads arrive as typed
// handler responses or as maps depending on the publisher, so they are read
// through their JSON form, which both share.
func parseTrigger(e events.Event) (trigger, bool) {
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return trigger{}, false
	}
	t := trigger{eventType: e.Type}
	if _, err := uuid.Parse(e.ActorID); err == nil && e.ActorType != "" {
		t.actorType, t.actorID = e.ActorType, e.ActorID
	}
	switch e.Type {
	case protocol.EventIssueCreated, protocol.EventIssueUpdated:
		var p struct {
			Issue struct {
				ID string `json:"id"`
				payloadIssue
			} `json:"issue"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return trigger{}, false
		}
		t.issueID = p.Issue.ID
		state := p.Issue.payloadIssue
		t.issueState = &state
	case protocol.EventCommentCreated:
		var p struct {
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		}
		if json.Unmarshal(raw, &p) != nil || p.Comment.ID == "" {
			return trigger{}, false
		}
		t.commentID = p.Comment.ID
	case protocol.EventTaskFailed:
		var p struct {
			TaskID       string `json:"task_id"`
			IssueID      string `json:"issue_id"`
			RetryPending bool   `json:"retry_pending"`
		}
		// A failure with a retry queued is not the end of the run.
		if json.Unmarshal(raw, &p) != nil || p.TaskID == "" || p.RetryPending {
			return trigger{}, false
		}
		t.taskID, t.issueID = p.TaskID, p.IssueID
	default:
		return trigger{}, false
	}
	if t.issueID == "" && t.commentID == "" {
		return trigger{}, false
	}
	return t, true
}

// issueRow is the issue state capture compares and records.
type issueRow struct {
	ID           string
	ProjectID    *string
	Status       string
	AssigneeType *string
	AssigneeID   *string
	CreatorType  string
	CreatorID    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func loadIssue(ctx context.Context, q Querier, workspaceID, issueID string) (issueRow, error) {
	var r issueRow
	err := q.QueryRow(ctx, `
		SELECT id::text, project_id::text, status, assignee_type, assignee_id::text,
		       creator_type, creator_id::text, created_at, updated_at
		FROM issue WHERE id = $1::uuid AND workspace_id = $2::uuid`, issueID, workspaceID,
	).Scan(&r.ID, &r.ProjectID, &r.Status, &r.AssigneeType, &r.AssigneeID,
		&r.CreatorType, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ancestorChain returns the issue and its parents, nearest first.
func ancestorChain(ctx context.Context, q Querier, issueID string) ([]string, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE up AS (
		    SELECT id, parent_issue_id, 0 AS depth FROM issue WHERE id = $1::uuid
		    UNION ALL
		    SELECT p.id, p.parent_issue_id, u.depth + 1
		    FROM up u JOIN issue p ON p.id = u.parent_issue_id
		    WHERE u.depth < 32
		)
		SELECT id::text FROM up ORDER BY depth`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chain []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		chain = append(chain, id)
	}
	return chain, rows.Err()
}

// pendingEvent is a router_event row before insertion.
type pendingEvent struct {
	Key        string
	Kind       string
	IssueID    string
	ActorType  *string
	ActorID    *string
	Payload    map[string]any
	OccurredAt time.Time
}

func (c *Capturer) capture(ctx context.Context, workspaceID string, subs []scopeSub, t trigger) error {
	q := c.Store.DB
	var comment struct {
		IssueID    string
		AuthorType string
		AuthorID   string
		ParentID   *string
		CreatedAt  time.Time
	}
	if t.commentID != "" {
		err := q.QueryRow(ctx, `
			SELECT issue_id::text, author_type, author_id::text, parent_id::text, created_at
			FROM comment WHERE id = $1::uuid AND workspace_id = $2::uuid`, t.commentID, workspaceID,
		).Scan(&comment.IssueID, &comment.AuthorType, &comment.AuthorID, &comment.ParentID, &comment.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		t.issueID = comment.IssueID
	}
	var task struct {
		AgentID       string
		FailureReason *string
		CompletedAt   *time.Time
	}
	if t.taskID != "" {
		err := q.QueryRow(ctx, `
			SELECT agent_id::text, failure_reason, completed_at
			FROM agent_task_queue WHERE id = $1::uuid`, t.taskID,
		).Scan(&task.AgentID, &task.FailureReason, &task.CompletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}

	issue, err := loadIssue(ctx, q, workspaceID, t.issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	chain, err := ancestorChain(ctx, q, issue.ID)
	if err != nil {
		return err
	}
	project := ""
	if issue.ProjectID != nil {
		project = *issue.ProjectID
	}
	matched := pickNarrowest(subs, chain, project)
	if len(matched) == 0 {
		return nil
	}

	tx, err := c.Store.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, sub := range matched {
		var evs []pendingEvent
		switch {
		case t.commentID != "":
			at, aid := comment.AuthorType, comment.AuthorID
			payload := map[string]any{"comment_id": t.commentID}
			if comment.ParentID != nil {
				payload["parent_id"] = *comment.ParentID
			}
			evs = append(evs, pendingEvent{
				Key: commentKey(t.commentID), Kind: KindCommentCreated, IssueID: issue.ID,
				ActorType: &at, ActorID: &aid, Payload: payload, OccurredAt: comment.CreatedAt,
			})
		case t.taskID != "":
			at, aid := "agent", task.AgentID
			payload := map[string]any{"task_id": t.taskID}
			if task.FailureReason != nil {
				payload["failure_reason"] = *task.FailureReason
			}
			occurred := time.Now()
			if task.CompletedAt != nil {
				occurred = *task.CompletedAt
			}
			evs = append(evs, pendingEvent{
				Key: taskFailedKey(t.taskID), Kind: KindTaskFailed, IssueID: issue.ID,
				ActorType: &at, ActorID: &aid, Payload: payload, OccurredAt: occurred,
			})
		}

		issueTriggered := t.eventType == protocol.EventIssueCreated || t.eventType == protocol.EventIssueUpdated
		if t.eventType == protocol.EventIssueCreated {
			ct, cid := issue.CreatorType, issue.CreatorID
			evs = append(evs, pendingEvent{
				Key: issueCreatedKey(issue.ID), Kind: KindIssueCreated, IssueID: issue.ID,
				ActorType: &ct, ActorID: &cid, Payload: map[string]any{}, OccurredAt: issue.CreatedAt,
			})
		}
		levelEvents, err := recordLevel(ctx, tx, workspaceID, sub, issue.ID, levelInput{
			update:    issueTriggered,
			attribute: t.attribute,
		})
		if err != nil {
			return err
		}
		evs = append(evs, levelEvents...)
		if _, err := insertEvents(ctx, tx, workspaceID, sub, evs, SourceBus); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// attribute credits a level change to the event's actor only when the event
// describes the state the change was found in; otherwise the actor is
// unknown.
func (t trigger) attribute(cur issueRow) (*string, *string) {
	if t.actorType == "" || t.actorID == "" || !t.issueState.describes(cur) {
		return nil, nil
	}
	actorType, actorID := t.actorType, t.actorID
	return &actorType, &actorID
}

type levelInput struct {
	// update moves the seen row to the current state. Without it (a comment
	// or task event, which says nothing about the issue's fields) only a
	// missing baseline is created, so an unpublished status change stays
	// visible to reconcile.
	update bool
	// attribute names who made a change found under the lock; nil means
	// unknown.
	attribute func(issueRow) (*string, *string)
	// before, when set, leaves alone an issue updated at or after it.
	before time.Time
}

// testHookBeforeSeenLock lets a test pause a capture between its earlier reads
// and the seen lock. Nil outside tests.
var testHookBeforeSeenLock func(subscriptionID, issueID string)

// recordLevel compares the issue's status and assignee with what the
// subscription last saw and returns the changes.
//
// The issue is read only after the seen row is locked, in the same
// transaction. Every capture and reconcile of a (subscription, issue) pair
// takes that lock first, so they compare in lock order against the newest
// committed state; a reader holding an older snapshot can never write the
// seen row back to it.
func recordLevel(ctx context.Context, tx pgx.Tx, workspaceID string, sub scopeSub, issueID string, in levelInput) ([]pendingEvent, error) {
	if testHookBeforeSeenLock != nil {
		testHookBeforeSeenLock(sub.ID, issueID)
	}
	// A missing row is created from the current issue state as a baseline,
	// never an event. Inserting it first means the row always exists to be
	// locked below.
	tag, err := tx.Exec(ctx, `
		INSERT INTO router_issue_seen (subscription_id, issue_id, workspace_id, status, assignee_type, assignee_id)
		SELECT $1::uuid, i.id, i.workspace_id, i.status, i.assignee_type, i.assignee_id
		FROM issue i WHERE i.id = $2::uuid AND i.workspace_id = $3::uuid
		ON CONFLICT (subscription_id, issue_id) DO NOTHING`, sub.ID, issueID, workspaceID)
	if err != nil || tag.RowsAffected() > 0 || !in.update {
		return nil, err
	}

	var seenStatus string
	var seenAssigneeType, seenAssigneeID *string
	err = tx.QueryRow(ctx, `
		SELECT status, assignee_type, assignee_id::text
		FROM router_issue_seen
		WHERE subscription_id = $1::uuid AND issue_id = $2::uuid
		FOR UPDATE`, sub.ID, issueID,
	).Scan(&seenStatus, &seenAssigneeType, &seenAssigneeID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The issue is gone, or its baseline was dropped concurrently.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	issue, err := loadIssue(ctx, tx, workspaceID, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !in.before.IsZero() && !issue.UpdatedAt.Before(in.before) {
		return nil, nil
	}
	var actorType, actorID *string
	if in.attribute != nil {
		actorType, actorID = in.attribute(issue)
	}
	changes := levelChanges(issue, seenStatus, seenAssigneeType, seenAssigneeID, actorType, actorID)
	if len(changes) == 0 {
		return nil, nil
	}
	_, err = tx.Exec(ctx, `
		UPDATE router_issue_seen
		SET status = $3, assignee_type = $4, assignee_id = $5::uuid, seen_at = now()
		WHERE subscription_id = $1::uuid AND issue_id = $2::uuid`,
		sub.ID, issueID, issue.Status, issue.AssigneeType, issue.AssigneeID)
	return changes, err
}

func levelChanges(issue issueRow, seenStatus string, seenAssigneeType, seenAssigneeID *string, actorType, actorID *string) []pendingEvent {
	var out []pendingEvent
	if seenStatus != issue.Status {
		out = append(out, pendingEvent{
			Key: levelChangeKey(issue.ID, KindStatusChanged, issue.UpdatedAt), Kind: KindStatusChanged,
			IssueID: issue.ID, ActorType: actorType, ActorID: actorID, OccurredAt: issue.UpdatedAt,
			Payload: map[string]any{"from": seenStatus, "to": issue.Status},
		})
	}
	if !sameRef(seenAssigneeType, issue.AssigneeType) || !sameRef(seenAssigneeID, issue.AssigneeID) {
		out = append(out, pendingEvent{
			Key: levelChangeKey(issue.ID, KindAssigneeChanged, issue.UpdatedAt), Kind: KindAssigneeChanged,
			IssueID: issue.ID, ActorType: actorType, ActorID: actorID, OccurredAt: issue.UpdatedAt,
			Payload: map[string]any{
				"from": assigneeRef(seenAssigneeType, seenAssigneeID),
				"to":   assigneeRef(issue.AssigneeType, issue.AssigneeID),
			},
		})
	}
	return out
}

func sameRef(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func assigneeRef(typ, id *string) any {
	if typ == nil || id == nil {
		return nil
	}
	return map[string]string{"type": *typ, "id": *id}
}

// insertEvents writes the subscription's events, skipping kinds it does not
// want and keys the agent already has. It returns how many rows landed.
//
// A row is written only while its subscription is still enabled: a capture
// working from a cache snapshot taken before a pause or delete then writes
// nothing. A pause or delete that commits during this transaction can still
// leave a row behind; it stays listed under the subscription id until the
// workspace is deleted.
func insertEvents(ctx context.Context, tx pgx.Tx, workspaceID string, sub scopeSub, evs []pendingEvent, source string) (int, error) {
	inserted := 0
	for _, ev := range evs {
		if !sub.wants(ev.Kind) {
			continue
		}
		payload, err := json.Marshal(ev.Payload)
		if err != nil {
			return inserted, err
		}
		if ev.Payload == nil {
			payload = []byte("{}")
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO router_event (
			    workspace_id, subscription_id, agent_id, event_key, kind, issue_id,
			    actor_type, actor_id, source, is_self, payload, occurred_at
			)
			SELECT $1::uuid, $2::uuid, $3::uuid, $4, $5, $6::uuid, $7, $8::uuid, $9, $10, $11, $12
			WHERE EXISTS (SELECT 1 FROM router_subscription WHERE id = $2::uuid AND enabled)
			ON CONFLICT (agent_id, event_key) DO NOTHING`,
			workspaceID, sub.ID, sub.AgentID, ev.Key, ev.Kind, ev.IssueID,
			ev.ActorType, ev.ActorID, source, isSelf(ev.ActorType, ev.ActorID, sub.AgentID), payload, ev.OccurredAt)
		if err != nil {
			return inserted, err
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}
