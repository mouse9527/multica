// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), Router v6 slices 1 (M-96) and 2 (M-97), Router parallelism (M-104), the Router's boundary (M-112), no rate pause (M-111), self-contained owner decisions (M-162), the chat wakeup and batch split (M-164), what "subscribed" means (M-175), own actions by run (M-193), quiet batch replies (M-210), the reply format (M-206), and wakeups on issues in scope (M-208).

package agentrouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunEnqueuer starts a Router run: an anchor run on an issue, or a chat turn
// that reads the batch written into a chat session (see chat.go). The
// server's implementation is TaskRuns, which goes through the task service;
// tests substitute their own.
type RunEnqueuer interface {
	EnqueueRouterRun(ctx context.Context, issueID, agentID, note string) (taskID string, err error)
	EnqueueRouterChatRun(ctx context.Context, chatSessionID, agentID, anchorIssueID, batch string) (taskID string, err error)
}

var (
	// ErrRunBusy: the run cannot start now (the agent already has a run
	// waiting on the anchor issue, or the issue is in triage). The batch
	// waits for the next tick.
	ErrRunBusy = errors.New("router: the anchor run cannot start yet")
	// ErrRunRefused: the run will not start until someone changes something
	// (project access, attribution policy). Dispatch pauses and tells the
	// owner.
	ErrRunRefused = errors.New("router: the anchor run was refused")
)

const (
	// maxBatchEvents bounds how many pending events one run takes. The rest
	// go to the next batch.
	maxBatchEvents = 200
	// maxDueSubscriptions bounds how many subscriptions one tick dispatches.
	maxDueSubscriptions = 100
)

// Dispatcher turns a subscription's pending events into anchor runs.
//
// Each tick it first settles finished runs (a run that failed or was
// cancelled, with every system retry of it, hands its batch back), then
// delivers notices nobody delivered yet, then dispatches every subscription
// whose batch is due: no new event for debounce_seconds, or the oldest
// pending event waited max_wait_seconds. Only events from others count: an
// event the subscribed agent caused (is_self) never starts a run, but rides
// along with the next batch.
//
// One run at a time per Router, not per agent: an agent can serve several
// Routers (subscriptions), and different Routers run in parallel up to the
// agent's max_concurrent_tasks and its runtime's cap, which the claim
// enforces; runs over the limit wait in the queue with their batch. A Router
// is its chat session when the subscription is bound to one (every
// subscription bound to that chat shares its conversation), and the
// subscription otherwise. A dispatch takes transaction-scoped advisory locks
// on the subscription and, when bound, on the chat, and gives up when another
// dispatch holds one or the Router has an open run (see openRun).
type Dispatcher struct {
	Store   *Store
	Runs    RunEnqueuer
	Notices *Notifier
	Logger  *slog.Logger
	// Chat sends a chat-bound subscription's batches to its chat (the
	// router_chat feature). Off, every subscription gets anchor runs.
	Chat bool
	// HoldShadow lists, in each chat batch, the inbox items the hold would
	// have taken since the last batch, and prunes old shadow rows (the
	// router_hold_shadow feature, hold.go).
	HoldShadow bool
	// Quiet tells chat batches how to end without alerting the owner (the
	// router_chat_quiet feature, chat_quiet.go).
	Quiet bool

	lastPrune atomic.Int64
}

func NewDispatcher(store *Store, runs RunEnqueuer, notices *Notifier) *Dispatcher {
	return &Dispatcher{Store: store, Runs: runs, Notices: notices, Logger: slog.Default()}
}

// DispatchResult counts what a tick did.
type DispatchResult struct {
	Settled    int `json:"settled"`
	Released   int `json:"released"`
	Dispatched int `json:"dispatched"`
	Paused     int `json:"paused"`
	Waiting    int `json:"waiting"`
	Notices    int `json:"notices"`
}

// Tick runs one dispatch pass over every workspace.
func (d *Dispatcher) Tick(ctx context.Context) (DispatchResult, error) {
	return d.TickWorkspace(ctx, "")
}

// TickWorkspace runs one dispatch pass over one workspace, or every
// workspace when workspaceID is empty.
func (d *Dispatcher) TickWorkspace(ctx context.Context, workspaceID string) (DispatchResult, error) {
	var res DispatchResult
	settled, released, err := d.settle(ctx, workspaceID)
	res.Settled, res.Released = settled, released
	if err != nil {
		return res, fmt.Errorf("settle runs: %w", err)
	}
	if d.Notices != nil {
		n, err := d.Notices.DeliverPending(ctx, workspaceID)
		res.Notices = n
		if err != nil {
			d.Logger.Warn("router: deliver pending notices failed", "error", err)
		}
	}
	if d.HoldShadow {
		d.pruneHolds(ctx)
	}
	if d.Chat {
		n, err := d.pauseUnusableChats(ctx, workspaceID)
		res.Paused += n
		if err != nil {
			d.Logger.Warn("router: pausing subscriptions with an unusable chat failed", "error", err)
		}
	}
	if _, err := d.liftRatePauses(ctx, workspaceID); err != nil {
		d.Logger.Warn("router: lifting rate pauses failed", "error", err)
	}
	due, err := d.dueSubscriptions(ctx, workspaceID)
	if err != nil {
		return res, fmt.Errorf("find due subscriptions: %w", err)
	}
	var errs []error
	for _, sub := range due {
		outcome, notice, err := d.dispatchOne(ctx, sub)
		if err != nil {
			d.Logger.Warn("router: dispatch failed; the batch stays queued",
				"subscription_id", sub.ID, "agent_id", sub.AgentID, "error", err)
			errs = append(errs, err)
			continue
		}
		if notice != "" && d.Notices != nil {
			d.Notices.Deliver(ctx, notice)
		}
		switch outcome {
		case outcomeDispatched:
			res.Dispatched++
		case outcomePaused:
			res.Paused++
		default:
			res.Waiting++
		}
	}
	return res, errors.Join(errs...)
}

// pruneHolds deletes old shadow rows, at most once an hour per process.
func (d *Dispatcher) pruneHolds(ctx context.Context) {
	last := d.lastPrune.Load()
	now := time.Now().Unix()
	if now-last < int64(time.Hour/time.Second) || !d.lastPrune.CompareAndSwap(last, now) {
		return
	}
	if n, err := pruneShadowHolds(ctx, d.Store.DB); err != nil {
		d.Logger.Warn("router: pruning shadow holds failed", "error", err)
	} else if n > 0 {
		d.Logger.Info("router: pruned shadow holds", "rows", n)
	}
}

// settle closes open dispatch rows whose run, followed through its system
// retries (retry_of_task_id), has finished. A chain that never completed
// hands its events back: they become pending again and join the next batch.
// A run that no longer exists counts as failed.
func (d *Dispatcher) settle(ctx context.Context, workspaceID string) (settled, released int, err error) {
	rows, err := d.Store.DB.Query(ctx, `
		WITH RECURSIVE chain AS (
		    SELECT dp.id AS dispatch_id, t.id AS task_id, COALESCE(t.status, 'cancelled') AS status
		    FROM router_dispatch dp
		    LEFT JOIN agent_task_queue t ON t.id = dp.task_id
		    WHERE dp.settled_at IS NULL AND ($1 = '' OR dp.workspace_id = NULLIF($1, '')::uuid)
		    UNION ALL
		    SELECT c.dispatch_id, t.id, t.status
		    FROM chain c
		    JOIN agent_task_queue t ON t.parent_task_id = c.task_id AND t.retry_of_task_id = c.task_id
		)
		SELECT dispatch_id::text, bool_or(status = 'completed')
		FROM chain
		GROUP BY dispatch_id
		HAVING NOT bool_or(status NOT IN ('completed', 'failed', 'cancelled'))`, workspaceID)
	if err != nil {
		return 0, 0, err
	}
	type done struct {
		id        string
		completed bool
	}
	finished, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (done, error) {
		var x done
		err := r.Scan(&x.id, &x.completed)
		return x, err
	})
	if err != nil {
		return 0, 0, err
	}
	for _, f := range finished {
		n, ok, err := d.settleOne(ctx, f.id, f.completed)
		if err != nil {
			return settled, released, err
		}
		if ok {
			settled++
			released += n
		}
	}
	return settled, released, nil
}

func (d *Dispatcher) settleOne(ctx context.Context, dispatchID string, completed bool) (int, bool, error) {
	tx, err := d.Store.Tx.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	outcome := "completed"
	if !completed {
		outcome = "failed"
	}
	var taskID string
	err = tx.QueryRow(ctx, `
		UPDATE router_dispatch SET outcome = $2, settled_at = now()
		WHERE id = $1::uuid AND settled_at IS NULL
		RETURNING task_id::text`, dispatchID, outcome).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another tick settled it.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	released := 0
	if !completed {
		tag, err := tx.Exec(ctx, `UPDATE router_event SET consumed_task_id = NULL WHERE consumed_task_id = $1::uuid`, taskID)
		if err != nil {
			return 0, false, err
		}
		released = int(tag.RowsAffected())
		// The hold rows the failed batch listed are listed again.
		if _, err := tx.Exec(ctx, `UPDATE router_hold SET dispatch_task_id = NULL WHERE dispatch_task_id = $1::uuid`, taskID); err != nil {
			return 0, false, err
		}
	}
	return released, true, tx.Commit(ctx)
}

// dueSub is a subscription whose batch is due.
type dueSub struct {
	ID          string
	WorkspaceID string
	AgentID     string
}

func (d *Dispatcher) dueSubscriptions(ctx context.Context, workspaceID string) ([]dueSub, error) {
	rows, err := d.Store.DB.Query(ctx, `
		SELECT s.id::text, s.workspace_id::text, s.agent_id::text
		FROM router_subscription s
		JOIN LATERAL (
		    SELECT min(e.created_at) AS first_at, max(e.created_at) AS last_at
		    FROM router_event e
		    WHERE e.subscription_id = s.id AND e.consumed_task_id IS NULL AND NOT e.is_self
		) p ON p.first_at IS NOT NULL
		WHERE s.enabled AND s.paused_reason IS NULL AND s.anchor_issue_id IS NOT NULL
		  AND ($2 = '' OR s.workspace_id = NULLIF($2, '')::uuid)
		  AND (p.last_at <= now() - make_interval(secs => s.debounce_seconds)
		       OR p.first_at <= now() - make_interval(secs => s.max_wait_seconds))
		ORDER BY p.first_at
		LIMIT $1`, maxDueSubscriptions, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (dueSub, error) {
		var s dueSub
		err := r.Scan(&s.ID, &s.WorkspaceID, &s.AgentID)
		return s, err
	})
}

type dispatchOutcome int

const (
	outcomeWaiting dispatchOutcome = iota
	outcomeDispatched
	outcomePaused
)

// dispatchSub is the subscription as dispatch reads it under the lock.
type dispatchSub struct {
	ID             string
	WorkspaceID    string
	AgentID        string
	OwnerUserID    *string
	ScopeType      string
	ScopeID        string
	AnchorIssueID  *string
	MaxRunsPerHour int
	Enabled        bool
	PausedReason   *string
	State          string
	StateRevision  int64
}

// dispatchOne dispatches one due subscription. It returns the outcome and,
// when it paused the subscription or found it busy, the id of the notice to
// deliver after the commit.
func (d *Dispatcher) dispatchOne(ctx context.Context, due dueSub) (dispatchOutcome, string, error) {
	tx, err := d.Store.Tx.Begin(ctx)
	if err != nil {
		return outcomeWaiting, "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if locked, err := tryLock(ctx, tx, "router:sub:", due.ID); err != nil || !locked {
		return outcomeWaiting, "", err
	}
	sub, err := lockSubscription(ctx, tx, due.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return outcomeWaiting, "", nil
	}
	if err != nil {
		return outcomeWaiting, "", err
	}
	if !sub.Enabled || sub.PausedReason != nil || sub.AnchorIssueID == nil {
		return outcomeWaiting, "", nil
	}
	// The binding is read under the subscription's row lock, which an update
	// of the binding also takes, so it cannot change before the commit.
	chatID := ""
	if d.Chat {
		if chatID, err = chatAnchorOf(ctx, tx, sub.ID); err != nil {
			return outcomeWaiting, "", err
		}
	}
	if chatID != "" {
		if locked, err := tryLock(ctx, tx, "router:chat:", chatID); err != nil || !locked {
			return outcomeWaiting, "", err
		}
	}
	if open, err := openRun(ctx, tx, sub, chatID); err != nil || open {
		return outcomeWaiting, "", err
	}

	var agentName, agentOwner string
	var archived, hasRuntime bool
	err = tx.QueryRow(ctx, `
		SELECT name, COALESCE(owner_id::text, ''), archived_at IS NOT NULL, runtime_id IS NOT NULL
		FROM agent WHERE id = $1::uuid AND workspace_id = $2::uuid`, sub.AgentID, sub.WorkspaceID,
	).Scan(&agentName, &agentOwner, &archived, &hasRuntime)
	if errors.Is(err, pgx.ErrNoRows) {
		archived = true
	} else if err != nil {
		return outcomeWaiting, "", err
	}
	pause := func(reason, why string) (dispatchOutcome, string, error) {
		id, err := d.pauseDispatch(ctx, tx, sub, agentName, agentOwner, reason, why)
		if err != nil {
			return outcomeWaiting, "", err
		}
		return outcomePaused, id, tx.Commit(ctx)
	}
	switch {
	case archived:
		return pause(PausedAgentUnavailable, "the agent is archived or gone")
	case !hasRuntime:
		return pause(PausedAgentUnavailable, "the agent has no runtime")
	}

	var anchorKey, anchorTitle string
	err = tx.QueryRow(ctx, `
		SELECT w.issue_prefix || '-' || i.number, i.title
		FROM issue i JOIN workspace w ON w.id = i.workspace_id
		WHERE i.id = $1::uuid AND i.workspace_id = $2::uuid`, *sub.AnchorIssueID, sub.WorkspaceID,
	).Scan(&anchorKey, &anchorTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		return pause(PausedAgentUnavailable, "the anchor issue no longer exists; set a new one with `multica router update --anchor`")
	}
	if err != nil {
		return outcomeWaiting, "", err
	}

	batch, err := loadBatch(ctx, tx, sub.ID)
	if err != nil {
		return outcomeWaiting, "", err
	}
	if !batch.hasOthers() {
		return outcomeWaiting, "", tx.Commit(ctx)
	}
	inflight, total, err := loadInFlight(ctx, tx, sub.WorkspaceID, sub.ID)
	if err != nil {
		return outcomeWaiting, "", err
	}
	var shadow []shadowHold
	shadowTotal := 0
	if chatID != "" && d.HoldShadow {
		if shadow, shadowTotal, err = loadShadowHolds(ctx, tx, sub.ID); err != nil {
			return outcomeWaiting, "", err
		}
	}
	note := buildNote(noteInput{
		sub: sub, anchorKey: anchorKey, scopeLabel: scopeLabel(ctx, tx, sub),
		batch: batch, inflight: inflight, inflightTotal: total, now: time.Now(), chat: chatID != "", quiet: d.Quiet,
		shadow: shadow, shadowTotal: shadowTotal,
	})

	if testHookBeforeEnqueue != nil {
		testHookBeforeEnqueue(sub.ID)
	}
	var taskID string
	if chatID != "" {
		taskID, err = d.Runs.EnqueueRouterChatRun(ctx, chatID, sub.AgentID, *sub.AnchorIssueID, note)
	} else {
		taskID, err = d.Runs.EnqueueRouterRun(ctx, *sub.AnchorIssueID, sub.AgentID, note)
	}
	switch {
	case errors.Is(err, ErrRunBusy):
		return outcomeWaiting, "", tx.Commit(ctx)
	case errors.Is(err, ErrChatUnavailable):
		return pause(PausedAgentUnavailable, chatPauseWhy(sub.ID, chatID, "cannot take batches ("+err.Error()+")"))
	case errors.Is(err, ErrRunRefused):
		return pause(PausedAgentUnavailable, err.Error())
	case err != nil:
		return outcomeWaiting, "", err
	}
	// The run exists from here on. If this commit fails, the batch stays
	// pending and a later batch carries the same events again: delivery is
	// at least once.
	if _, err := tx.Exec(ctx, `
		INSERT INTO router_dispatch (workspace_id, subscription_id, agent_id, anchor_issue_id, task_id, event_count)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6)`,
		sub.WorkspaceID, sub.ID, sub.AgentID, *sub.AnchorIssueID, taskID, len(batch.events)); err != nil {
		return outcomeWaiting, "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE router_event SET consumed_task_id = $2::uuid
		WHERE id = ANY($1::uuid[]) AND consumed_task_id IS NULL`, batch.ids(), taskID); err != nil {
		return outcomeWaiting, "", err
	}
	if len(shadow) > 0 {
		ids := make([]string, len(shadow))
		for i, x := range shadow {
			ids[i] = x.ID
		}
		if _, err := tx.Exec(ctx, `
			UPDATE router_hold SET dispatch_task_id = $2::uuid
			WHERE id = ANY($1::uuid[]) AND dispatch_task_id IS NULL`, ids, taskID); err != nil {
			return outcomeWaiting, "", err
		}
	}
	notice, err := d.alertBusy(ctx, tx, sub, agentName, agentOwner)
	if err != nil {
		return outcomeWaiting, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return outcomeWaiting, "", err
	}
	d.Logger.Info("router: run queued", "subscription_id", sub.ID, "agent_id", sub.AgentID,
		"task_id", taskID, "chat_session_id", chatID, "events", len(batch.events))
	return outcomeDispatched, notice, nil
}

// alertBusy tells the owner, at most once per subscription and hour, that the
// Router started more runs in the last hour than max_runs_per_hour, the run
// just queued included. It only tells: dispatch goes on, because a Router
// keeps working for as long as its scope has events (M-111). It returns the
// id of the notice to deliver after the commit, or "".
func (d *Dispatcher) alertBusy(ctx context.Context, tx pgx.Tx, sub dispatchSub, agentName, agentOwner string) (string, error) {
	var recent int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM router_dispatch
		WHERE subscription_id = $1::uuid AND created_at > now() - interval '1 hour'`, sub.ID).Scan(&recent); err != nil {
		return "", err
	}
	recipient := noticeRecipient(sub, agentOwner)
	if recent <= sub.MaxRunsPerHour || recipient == "" {
		return "", nil
	}
	n, created, err := insertNotice(ctx, tx, NoticeInput{
		WorkspaceID:     sub.WorkspaceID,
		RecipientUserID: recipient,
		Title:           fmt.Sprintf("Super Agent busy for %s", agentName),
		Body: fmt.Sprintf("Super Agent subscription %s started %d runs in the last hour, more than its alert threshold of %d "+
			"(max_runs_per_hour). It keeps dispatching; nothing is paused. If this is more than the scope's activity explains, "+
			"look at `multica router runs --subscription %s`; raise the threshold with `multica router update %s --max-runs-per-hour <n>`, "+
			"or stop it with `multica router pause %s`.", sub.ID, recent, sub.MaxRunsPerHour, sub.ID, sub.ID, sub.ID),
		Refs:                 anchorRefs(sub),
		Urgency:              UrgencyNormal,
		DedupeKey:            fmt.Sprintf("router_busy:%s:%s", sub.ID, time.Now().UTC().Format("2006010215")),
		SenderType:           "system",
		SourceSubscriptionID: sub.ID,
	})
	if err != nil || !created {
		return "", err
	}
	d.Logger.Info("router: busy", "subscription_id", sub.ID, "agent_id", sub.AgentID,
		"runs_last_hour", recent, "max_runs_per_hour", sub.MaxRunsPerHour)
	return n.ID, nil
}

// testHookBeforeEnqueue, when set by a test, runs with the dispatch's locks
// held, just before the run is queued.
var testHookBeforeEnqueue func(subscriptionID string)

// tryLock takes a transaction-scoped advisory lock on prefix+id without
// waiting, and reports whether it got it.
func tryLock(ctx context.Context, tx pgx.Tx, prefix, id string) (bool, error) {
	var locked bool
	err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1 || $2, 0))`, prefix, id).Scan(&locked)
	return locked, err
}

// openRun reports whether the Router already has a run that has not been
// settled: one of the subscription's own (an anchor run started before it
// was bound to a chat counts too), or, when it is bound to chatID, a batch
// turn in that chat from any subscription. The agent's runs for other
// Routers do not count.
func openRun(ctx context.Context, tx pgx.Tx, sub dispatchSub, chatID string) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM router_dispatch dp
		    LEFT JOIN agent_task_queue t ON t.id = dp.task_id
		    WHERE dp.agent_id = $1::uuid AND dp.settled_at IS NULL
		      AND (dp.subscription_id = $2::uuid OR ($3 <> '' AND t.chat_session_id = NULLIF($3, '')::uuid)))`,
		sub.AgentID, sub.ID, chatID).Scan(&open)
	return open, err
}

// lockSubscription reads a subscription for dispatch and locks its row.
func lockSubscription(ctx context.Context, tx pgx.Tx, id string) (dispatchSub, error) {
	var sub dispatchSub
	err := tx.QueryRow(ctx, `
		SELECT id::text, workspace_id::text, agent_id::text, owner_user_id::text, scope_type, scope_id::text,
		       anchor_issue_id::text, max_runs_per_hour, enabled, paused_reason, state, state_revision
		FROM router_subscription WHERE id = $1::uuid FOR UPDATE`, id,
	).Scan(&sub.ID, &sub.WorkspaceID, &sub.AgentID, &sub.OwnerUserID, &sub.ScopeType, &sub.ScopeID,
		&sub.AnchorIssueID, &sub.MaxRunsPerHour, &sub.Enabled, &sub.PausedReason, &sub.State, &sub.StateRevision)
	return sub, err
}

// pauseDispatch stops dispatch for a subscription (capture goes on), records
// it, and writes the notice for its owner in the same transaction. The notice
// is delivered after the commit; DeliverPending covers a process that stops
// in between. The dedupe key allows one notice per subscription, reason and
// hour.
func (d *Dispatcher) pauseDispatch(ctx context.Context, tx pgx.Tx, sub dispatchSub, agentName, agentOwner, reason, why string) (string, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE router_subscription SET paused_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND enabled AND paused_reason IS NULL`, sub.ID, reason); err != nil {
		return "", err
	}
	if err := recordChange(ctx, tx, sub.WorkspaceID, sub.ID, Change{
		Action: ChangeAutoPause, ActorType: "system",
		Detail: map[string]any{"reason": reason, "why": why},
	}); err != nil {
		return "", err
	}
	recipient := noticeRecipient(sub, agentOwner)
	d.Logger.Warn("router: dispatch paused", "subscription_id", sub.ID, "agent_id", sub.AgentID,
		"reason", reason, "why", why)
	if recipient == "" {
		return "", nil
	}
	var waiting int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM router_event
		WHERE subscription_id = $1::uuid AND consumed_task_id IS NULL AND NOT is_self`, sub.ID).Scan(&waiting); err != nil {
		return "", err
	}
	n, _, err := insertNotice(ctx, tx, NoticeInput{
		WorkspaceID:     sub.WorkspaceID,
		RecipientUserID: recipient,
		Title:           fmt.Sprintf("Super Agent paused for %s", agentName),
		Body: fmt.Sprintf("Super Agent subscription %s stopped starting runs for %s: %s. "+
			"%d event(s) are waiting, and events are still captured. Resume with `multica router resume %s`.",
			sub.ID, agentName, why, waiting, sub.ID),
		Refs:                 anchorRefs(sub),
		Urgency:              UrgencyHigh,
		DedupeKey:            fmt.Sprintf("router_paused:%s:%s:%s", sub.ID, reason, time.Now().UTC().Format("2006010215")),
		SenderType:           "system",
		SourceSubscriptionID: sub.ID,
	})
	if err != nil {
		return "", err
	}
	return n.ID, nil
}

// noticeRecipient is the member a subscription's system notices go to: its
// owner, or the subscribed agent's owner (see subscriptionOwnerSQL).
func noticeRecipient(sub dispatchSub, agentOwner string) string {
	if sub.OwnerUserID != nil {
		return *sub.OwnerUserID
	}
	return agentOwner
}

func anchorRefs(sub dispatchSub) []string {
	if sub.AnchorIssueID == nil {
		return nil
	}
	return []string{*sub.AnchorIssueID}
}

// liftRatePauses resumes dispatch for subscriptions an older release paused
// for "rate". Dispatch no longer pauses for the hourly run count (M-111), so
// this finds rows only after an upgrade, or while an older replica still runs
// during a rolling one.
func (d *Dispatcher) liftRatePauses(ctx context.Context, workspaceID string) (int, error) {
	tx, err := d.Store.Tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `
		UPDATE router_subscription SET paused_reason = NULL, updated_at = now()
		WHERE enabled AND paused_reason = $1 AND ($2 = '' OR workspace_id = NULLIF($2, '')::uuid)
		RETURNING id::text, workspace_id::text`, PausedRate, workspaceID)
	if err != nil {
		return 0, err
	}
	type lifted struct{ id, ws string }
	subs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lifted, error) {
		var x lifted
		err := r.Scan(&x.id, &x.ws)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	for _, s := range subs {
		if err := recordChange(ctx, tx, s.ws, s.id, Change{
			Action: ChangeResume, ActorType: "system",
			Detail: map[string]any{"reason": PausedRate, "why": "the Super Agent no longer pauses for its hourly run count"},
		}); err != nil {
			return 0, err
		}
		d.Logger.Info("router: lifted a rate pause", "subscription_id", s.id)
	}
	return len(subs), tx.Commit(ctx)
}

// batchEvent is one pending event as the note shows it.
type batchEvent struct {
	ID         string
	Kind       string
	IssueKey   string
	IssueTitle string
	ActorType  *string
	ActorName  string
	Self       bool
	Payload    map[string]any
	OccurredAt time.Time
}

// batch is what one run takes: up to maxBatchEvents events others caused,
// and the agent's own pending events, which are consumed with the batch but
// never shown.
type batch struct {
	events  []batchEvent
	selfIDs []string
}

func (b batch) hasOthers() bool { return len(b.events) > 0 }

func (b batch) ids() []string {
	out := make([]string, 0, len(b.events)+len(b.selfIDs))
	for _, e := range b.events {
		out = append(out, e.ID)
	}
	return append(out, b.selfIDs...)
}

// loadBatch reads the subscription's pending events. The limit applies to
// events others caused only, so any number of the agent's own events ahead
// of them cannot hold a batch back.
func loadBatch(ctx context.Context, q Querier, subscriptionID string) (batch, error) {
	rows, err := q.Query(ctx, `
		SELECT e.id::text, e.kind, COALESCE(w.issue_prefix || '-' || i.number, ''), COALESCE(i.title, ''),
		       e.actor_type, COALESCE(a.name, u.name, ''), e.is_self, e.payload, e.occurred_at
		FROM router_event e
		LEFT JOIN issue i ON i.id = e.issue_id
		LEFT JOIN workspace w ON w.id = i.workspace_id
		LEFT JOIN agent a ON e.actor_type = 'agent' AND a.id = e.actor_id
		LEFT JOIN "user" u ON e.actor_type = 'member' AND u.id = e.actor_id
		WHERE e.subscription_id = $1::uuid AND e.consumed_task_id IS NULL AND NOT e.is_self
		ORDER BY e.created_at, e.id
		LIMIT $2`, subscriptionID, maxBatchEvents)
	if err != nil {
		return batch{}, err
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (batchEvent, error) {
		var e batchEvent
		err := r.Scan(&e.ID, &e.Kind, &e.IssueKey, &e.IssueTitle, &e.ActorType, &e.ActorName, &e.Self, &e.Payload, &e.OccurredAt)
		return e, err
	})
	if err != nil || len(evs) == 0 {
		return batch{events: evs}, err
	}
	// A batch cut at the limit takes own events only up to its last event;
	// the rest go with the next batch.
	last := ""
	if len(evs) == maxBatchEvents {
		last = evs[len(evs)-1].ID
	}
	rows, err = q.Query(ctx, `
		SELECT e.id::text FROM router_event e
		WHERE e.subscription_id = $1::uuid AND e.consumed_task_id IS NULL AND e.is_self
		  AND ($2 = '' OR (e.created_at, e.id) <= (
		      SELECT l.created_at, l.id FROM router_event l WHERE l.id = NULLIF($2, '')::uuid))`, subscriptionID, last)
	if err != nil {
		return batch{}, err
	}
	self, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return batch{events: evs, selfIDs: self}, err
}

// inFlightIssue is one open issue in scope as the note shows it.
type inFlightIssue struct {
	Key          string
	Title        string
	Status       string
	AssigneeType *string
	AssigneeName string
	UpdatedAt    time.Time
}

const maxInFlight = 40

// loadInFlight lists the subscription's open issues, most recently updated
// first, and how many there are in all.
func loadInFlight(ctx context.Context, q Querier, workspaceID, subscriptionID string) ([]inFlightIssue, int, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE `+membersCTE+`
		SELECT w.issue_prefix || '-' || i.number, i.title, i.status, i.assignee_type,
		       COALESCE(a.name, u.name, ''), i.updated_at, count(*) OVER ()
		FROM members m
		JOIN issue i ON i.id = m.issue_id
		JOIN workspace w ON w.id = i.workspace_id
		LEFT JOIN agent a ON i.assignee_type = 'agent' AND a.id = i.assignee_id
		LEFT JOIN "user" u ON i.assignee_type = 'member' AND u.id = i.assignee_id
		WHERE m.sub_id = $2::uuid AND i.status NOT IN ('done', 'cancelled')
		ORDER BY i.updated_at DESC, i.id
		LIMIT $3`, workspaceID, subscriptionID, maxInFlight)
	if err != nil {
		return nil, 0, err
	}
	total := 0
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (inFlightIssue, error) {
		var x inFlightIssue
		err := r.Scan(&x.Key, &x.Title, &x.Status, &x.AssigneeType, &x.AssigneeName, &x.UpdatedAt, &total)
		return x, err
	})
	return out, total, err
}

// scopeLabel names the scope for the note: the root issue key or the
// project title.
func scopeLabel(ctx context.Context, q Querier, sub dispatchSub) string {
	var label string
	var err error
	switch sub.ScopeType {
	case ScopeIssueTree:
		err = q.QueryRow(ctx, `
			SELECT w.issue_prefix || '-' || i.number FROM issue i JOIN workspace w ON w.id = i.workspace_id
			WHERE i.id = $1::uuid`, sub.ScopeID).Scan(&label)
		label = "issue tree " + label
	default:
		err = q.QueryRow(ctx, `SELECT title FROM project WHERE id = $1::uuid`, sub.ScopeID).Scan(&label)
		label = "project " + label
	}
	if err != nil {
		return sub.ScopeType + " " + sub.ScopeID
	}
	return label
}

type noteInput struct {
	sub           dispatchSub
	anchorKey     string
	scopeLabel    string
	batch         batch
	inflight      []inFlightIssue
	inflightTotal int
	now           time.Time
	// chat: the note is written into the subscription's chat (M-96).
	chat bool
	// quiet: a chat batch can end with a quiet reply (M-210).
	quiet bool
	// shadow lists the inbox items the hold would have taken (M-97), of
	// shadowTotal not yet listed.
	shadow      []shadowHold
	shadowTotal int
}

const (
	maxNoteEventsBytes   = 6 * 1024
	maxNoteInFlightBytes = 6 * 1024
	maxNoteShadowBytes   = 3 * 1024
	maxNoteTitle         = 80
)

// routerBoundary keeps the Router to dispatching, tracking and relaying: the
// substantive work goes to workers through issues (M-112). The full guide is
// the CLI's `multica router guide` (M-150).
const routerBoundary = "You dispatch, track and relay; you do not do the work yourself. " +
	"Yours: reading state to decide and relay (issues, comments, runs, subscriptions, PR and Actions status) " +
	"and issue and project operations (create, assign, change status, comment to @ the handler, update Super Agent state). " +
	"Hand off everything else, including querying or analysing business data (databases, logs, metrics), research, root-cause diagnosis, " +
	"writing plans or docs, changing code or config, and any work whose result is itself the deliverable: " +
	"a small task becomes a local Multica issue assigned to a fitting worker; " +
	"work worth tracking in the repository (a code change that will produce a PR, a design conclusion that needs a public record) " +
	"also gets a GitHub issue, linked as `gh_issue` per the github-evidence skill; " +
	"ops, environment and credential work stays a local issue only. " +
	"The full guide, including the commands for this Super Agent's settings, is `multica router guide`.\n" +
	routerSubscribedMeans +
	routerWakeupSplit +
	routerOwnerDecisions +
	routerReplyFormat

// routerWakeupSplit: the batch already reports issues in scope, so a chat
// wakeup on them wakes the Super Agent twice for one change (M-164). A timed
// wakeup on such an issue is still right, but the owner saw it next to the
// batch reports and read it as a duplicate (M-208). The rule and its examples
// are in `multica router guide`.
const routerWakeupSplit = "Batches already report every issue in your scope: do not create a chat wakeup " +
	"(`--until-*` condition or `--every` check) for an issue in scope, and never poll one with `--every`. " +
	"A chat wakeup is still fine for issues outside the scope, one-off reminders (`--at` / `--after`), " +
	"and what batches never carry, such as a PR merging or an Actions result. " +
	"When you do create one on an issue in scope (a timed retry or reminder), say in your reply what kind it is and why batches do not cover it, " +
	"e.g. a timed retry, not a repeat of the issue's events. " +
	"In a turn a wakeup starts, read the issue's current status and latest comments before reporting: the wakeup was written earlier, " +
	"and a batch may have reported it already. " +
	"See \"chat wakeup 和事件批次的分工\" in `multica router guide`.\n"

// routerOwnerDecisions: the owner often reads only the decision list at the
// end of a reply, so each item has to be decidable on its own (M-162). The
// rule and its examples are in `multica router guide`.
const routerOwnerDecisions = "Every decision you ask of the owner must stand on its own: " +
	"say in plain words what it is and why the owner has to decide, give each option with its consequence (never bare letters), " +
	"your recommendation in one sentence, and what the approval does not cover; " +
	"list every open decision, and only those, each time you reply. " +
	"See \"请 Owner 决定的事\" in `multica router guide`.\n"

// chatBatchReplyRule says when a chat batch's reply alerts the owner. With
// quiet replies on, a reply that opens with QuietReplyMarker stays in the
// chat without the alert; the turn cannot end without text, as the runtime
// asks for a visible reply.
func chatBatchReplyRule(quiet bool) string {
	if !quiet {
		return "Any text you reply here notifies the owner: reply only when they need to know, decide or act, and say so plainly. " +
			"When nothing needs them, finish without any text reply; the turn stays silent.\n"
	}
	return "A reply here notifies the owner unless its first line is exactly `" + QuietReplyMarker + "`. " +
		"When the batch brings nothing new for the owner to decide or act on, start the reply with that line and add at most one short line on what happened; " +
		"it stays in this chat without notifying them, and does not repeat the open decisions. " +
		"When the owner has something new to decide or act on, never use that line: reply plainly, and the owner is notified. " +
		"See \"没事可报的批次\" in `multica router guide`.\n"
}

// routerReplyFormat: the reporting format came only from per-workspace notes
// and the agent's memory, so a Super Agent in another workspace lost it
// (M-206). The rule and its examples are in `multica router guide`.
const routerReplyFormat = "Write every reply to the owner in the owner's language, and end it with a separate, numbered section " +
	"of what needs the owner, titled in that language (「需要你处理」 in Chinese): " +
	"repeat every open item each time, and write \"none for now\" (目前没有) when there is none. " +
	"Write times in the owner's time zone: convert the UTC (`...Z`) times the CLI and API return, and name the zone when you do not know theirs. " +
	"Merged is not fixed: when the fix for a problem the owner reported is merged but not deployed, " +
	"say it is merged, not live, and still happens in production, and list whether to deploy as an item for the owner. " +
	"Before relaying an issue, read its current status and latest comments; do not repeat an older conclusion. " +
	"A batch with nothing new for the owner gets no reply: do not send one only to repeat open items, " +
	"but whenever you do reply, list them all. " +
	"See \"每次回复的格式\" in `multica router guide`.\n"

// buildNote renders the run's context: the batch, the open issues in scope
// and the subscription's state, with the CLI commands that reach further.
// The daemon puts it in the run's prompt as the handoff note.
func buildNote(in noteInput) string {
	var b strings.Builder
	id := in.sub.ID
	others := 0
	for _, e := range in.batch.events {
		if !e.Self {
			others++
		}
	}
	if in.chat {
		b.WriteString(RouterBatchLabel + "\n")
	}
	fmt.Fprintf(&b, "Super Agent batch for subscription %s (%s, anchor %s): %d new event(s).\n", id, in.scopeLabel, in.anchorKey, others)
	if in.chat {
		b.WriteString("The Super Agent dispatcher wrote this message into your chat with the owner; the owner did not. " +
			"You are the Super Agent for this scope. Read the events, check current state where it matters, and act. " +
			"Your own actions are left out and never start a batch: those of your turns in this chat. " +
			"What your agent does in issue tasks or other chats comes in like anyone else's, so work you hand to your own agent is reported here too. " +
			chatBatchReplyRule(in.quiet) +
			routerBoundary +
			"When the owner asks you something here, answer directly only if one sentence from current state answers it; " +
			"otherwise open an issue, hand it off and reply with one line naming the issue and who has it.\n\n")
	} else {
		b.WriteString("You are the Super Agent for this scope. Read the events, check current state where it matters, and act. " +
			"Your own actions are left out and never start a run: those of your runs for this subscription. " +
			"What your agent does in other tasks or chats comes in like anyone else's, so work you hand to your own agent is reported here too. " +
			"When a person has to decide or act, tell them with `multica notify owner --subscription " + id + " --title ... --body ...`; do not rely on issue comments alone. " +
			"Write that body as you would a reply to the owner, in the format below.\n" +
			routerBoundary + "\n")
	}

	b.WriteString("Events, oldest first:\n")
	shown, size := 0, 0
	for _, e := range in.batch.events {
		if e.Self {
			continue
		}
		line := "- " + eventLine(e) + "\n"
		if size+len(line) > maxNoteEventsBytes {
			break
		}
		b.WriteString(line)
		size += len(line)
		shown++
	}
	if shown < others {
		fmt.Fprintf(&b, "- ... %d more: `multica router events --subscription %s --state pending --output json`\n", others-shown, id)
	}

	writeShadowHolds(&b, in)

	fmt.Fprintf(&b, "\nOpen issues in scope (%d of %d, most recently updated first):\n", len(in.inflight), in.inflightTotal)
	size = 0
	listed := 0
	for _, x := range in.inflight {
		assignee := "unassigned"
		if x.AssigneeType != nil {
			assignee = *x.AssigneeType + " " + x.AssigneeName
		}
		line := fmt.Sprintf("- %s [%s] %s, updated %s ago: %s\n", x.Key, x.Status, assignee,
			roundAge(in.now.Sub(x.UpdatedAt)), truncate(x.Title, maxNoteTitle))
		if size+len(line) > maxNoteInFlightBytes {
			break
		}
		b.WriteString(line)
		size += len(line)
		listed++
	}
	if listed < in.inflightTotal {
		fmt.Fprintf(&b, "- ... the rest: `multica router scope %s`\n", id)
	}

	fmt.Fprintf(&b, "\nState (revision %d). Keep what you need next time here: "+
		"`multica router state set %s --expect-revision %d --state-file <path>` (at most %d bytes; on a revision conflict read it again with `multica router state get %s` and merge).\n",
		in.sub.StateRevision, id, in.sub.StateRevision, MaxStateBytes, id)
	if strings.TrimSpace(in.sub.State) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(in.sub.State)
		if !strings.HasSuffix(in.sub.State, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// writeShadowHolds lists the inbox items the hold would have taken. They are
// in the owner's inbox already: the list shows the Router what the hold will
// bring it, and asks for nothing.
func writeShadowHolds(b *strings.Builder, in noteInput) {
	if len(in.shadow) == 0 {
		return
	}
	fmt.Fprintf(b, "\nHold preview (shadow mode, nothing to do): %d inbox item(s) for the owner in this scope since the last batch. "+
		"They went to the owner's inbox as usual; once the hold is on, items like these will come to you first:\n", in.shadowTotal)
	size, shown := 0, 0
	for _, x := range in.shadow {
		line := "- " + x.CreatedAt.UTC().Format(time.RFC3339) + " " + x.IssueKey + " " + x.ItemType
		if x.ActorType != nil {
			line += " by " + *x.ActorType
			if x.ActorName != "" {
				line += " " + x.ActorName
			}
		}
		line += ": " + truncate(x.Title, maxNoteTitle)
		if !x.InInbox {
			line += " (not in the inbox)"
		}
		line += "\n"
		if size+len(line) > maxNoteShadowBytes {
			break
		}
		b.WriteString(line)
		size += len(line)
		shown++
	}
	if shown < in.shadowTotal {
		fmt.Fprintf(b, "- ... %d more: `multica router holds --subscription %s`\n", in.shadowTotal-shown, in.sub.ID)
	}
}

func eventLine(e batchEvent) string {
	var b strings.Builder
	b.WriteString(e.OccurredAt.UTC().Format(time.RFC3339))
	b.WriteString(" ")
	b.WriteString(e.IssueKey)
	b.WriteString(" ")
	b.WriteString(e.Kind)
	switch e.Kind {
	case KindStatusChanged:
		fmt.Fprintf(&b, " %v -> %v", e.Payload["from"], e.Payload["to"])
	case KindAssigneeChanged:
		fmt.Fprintf(&b, " -> %s", assigneeText(e.Payload["to"]))
	case KindCommentCreated:
		if id, ok := e.Payload["comment_id"].(string); ok {
			fmt.Fprintf(&b, " comment %s", id)
		}
	case KindTaskFailed:
		if reason, ok := e.Payload["failure_reason"].(string); ok && reason != "" {
			fmt.Fprintf(&b, " (%s)", reason)
		}
	}
	if e.ActorType != nil {
		fmt.Fprintf(&b, " by %s", *e.ActorType)
		if e.ActorName != "" {
			fmt.Fprintf(&b, " %s", e.ActorName)
		}
	}
	if e.IssueTitle != "" {
		fmt.Fprintf(&b, ": %s", truncate(e.IssueTitle, maxNoteTitle))
	}
	return b.String()
}

func assigneeText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "unassigned"
	}
	return fmt.Sprintf("%v %v", m["type"], m["id"])
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func roundAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// DispatchRecord is one anchor run as read back.
type DispatchRecord struct {
	ID             string     `json:"id"`
	SubscriptionID string     `json:"subscription_id"`
	AgentID        string     `json:"agent_id"`
	AnchorIssueID  string     `json:"anchor_issue_id"`
	TaskID         string     `json:"task_id"`
	TaskStatus     *string    `json:"task_status"`
	EventCount     int        `json:"event_count"`
	Outcome        *string    `json:"outcome"`
	SettledAt      *time.Time `json:"settled_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ListDispatches returns a workspace's anchor runs, newest first, optionally
// for one subscription.
func (s *Store) ListDispatches(ctx context.Context, workspaceID, subscriptionID string, limit int) ([]DispatchRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.Query(ctx, `
		SELECT dp.id::text, dp.subscription_id::text, dp.agent_id::text, dp.anchor_issue_id::text,
		       dp.task_id::text, t.status, dp.event_count, dp.outcome, dp.settled_at, dp.created_at
		FROM router_dispatch dp
		LEFT JOIN agent_task_queue t ON t.id = dp.task_id
		WHERE dp.workspace_id = $1::uuid
		  AND ($2 = '' OR dp.subscription_id = NULLIF($2, '')::uuid)
		ORDER BY dp.created_at DESC, dp.id DESC
		LIMIT $3`, workspaceID, subscriptionID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DispatchRecord, error) {
		var d DispatchRecord
		err := r.Scan(&d.ID, &d.SubscriptionID, &d.AgentID, &d.AnchorIssueID, &d.TaskID, &d.TaskStatus,
			&d.EventCount, &d.Outcome, &d.SettledAt, &d.CreatedAt)
		return d, err
	})
}
