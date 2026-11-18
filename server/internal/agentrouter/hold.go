// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97), and Router inbox S3b (M-133).

package agentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The hold puts the Router between the inbox and its owner: an inbox item for
// the owner of a chat-bound subscription, about an issue in its scope, goes to
// the Router first. Slice 2 runs it in shadow mode only. HoldShadow is
// registered as the inbox interceptor (inbox.SetInterceptor); for every inbox
// write it decides whether the hold would take the item, records a 'shadow'
// router_hold row when it would, and lets the write go through unchanged.
// The next batch into the subscription's chat lists these items, and the
// interceptor keeps counts of its calls, latency and failures, so the cost
// and reach of the hold are known before slice 3 holds anything.
//
// What the hold takes (D4, M-95): an item for a member who owns an enabled,
// unpaused subscription bound to an active chat with its agent, whose issue
// is in that subscription's scope (the narrowest one when the member owns
// several), except the types in holdExemptTypes and items the Router's own
// agent caused. Items without an issue are never taken.

const (
	// DefaultHoldBudget bounds one interceptor call. The inbox write waits
	// at most this long, then goes ahead without the Router.
	DefaultHoldBudget = 250 * time.Millisecond

	HoldModeShadow   = "shadow"
	HoldStatusShadow = "shadow"

	// holdRetention is how long shadow rows are kept.
	holdRetention = 30 * 24 * time.Hour
)

// holdExemptTypes are inbox item types the hold never takes: the Router's own
// notices (including its pause and failure notices) and system items that are
// not about an issue's work (connected-app requests, autopilot and quota
// notices, quick-create outcomes).
var holdExemptTypes = map[string]bool{
	InboxNoticeType:            true,
	"app_action_requested":     true,
	"app_action_reminder":      true,
	"app_connect_requested":    true,
	"app_action_result":        true,
	"autopilot_paused":         true,
	"autopilot_quota_exceeded": true,
	"quick_create_failed":      true,
	"quick_create_unconfirmed": true,
}

// holdSub is a subscription the hold can route through: enabled, unpaused,
// bound to an active chat with its agent.
type holdSub struct {
	scopeSub
	OwnerUserID string
}

type holdSnapshot struct {
	// byOwner is keyed by workspace id and owner user id.
	byOwner  map[[2]string][]holdSub
	gen      int64
	loadedAt time.Time
}

func (s *holdSnapshot) forOwner(workspaceID, userID string) []holdSub {
	if s == nil {
		return nil
	}
	return s.byOwner[[2]string{workspaceID, userID}]
}

// HoldShadow is the shadow-mode inbox interceptor.
type HoldShadow struct {
	Store  *Store
	TTL    time.Duration
	Logger *slog.Logger

	// The subscription cache works like the Capturer's: lock-free reads, one
	// reloader at a time, backoff after a failed reload.
	snap           atomic.Pointer[holdSnapshot]
	reloading      atomic.Bool
	reloadFailures int
	retryAt        time.Time

	stats holdStats

	// rules, when set, judges every recorded item by the workspace's rule
	// list (inbox_rules.go). Nil, the rows carry no verdict.
	rules *inboxRules
}

// ClassifyByRules turns on rule verdicts for the items the hold records.
func (h *HoldShadow) ClassifyByRules() { h.rules = newInboxRules() }

func NewHoldShadow(store *Store) *HoldShadow {
	h := &HoldShadow{Store: store, TTL: DefaultCacheTTL, Logger: slog.Default()}
	h.stats.since = time.Now()
	return h
}

// activeHold is the interceptor this process registered, for the stats API.
var activeHold atomic.Pointer[HoldShadow]

// Intercept implements inbox.Interceptor. It never holds: it only records.
func (h *HoldShadow) Intercept(ctx context.Context, p db.CreateInboxItemParams) (hold bool, err error) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			h.stats.panics.Add(1)
			err = fmt.Errorf("router hold: panic: %v", r)
		}
		h.stats.observe(time.Since(start), err, ctx.Err() != nil)
		if err != nil {
			h.logFailure(p, err)
		}
	}()
	sub, ok, err := h.decide(ctx, p)
	if err != nil || !ok {
		return false, err
	}
	h.stats.wouldHold.Add(1)
	var note *string
	if h.rules != nil {
		var ruleErr error
		if note, ruleErr = h.rules.classify(ctx, h.Store.DB, p); ruleErr != nil {
			h.logRuleFailure(p, ruleErr)
		}
	}
	return false, h.recordShadow(ctx, sub, p, note, time.Since(start))
}

// decide returns the subscription the hold would route p through.
func (h *HoldShadow) decide(ctx context.Context, p db.CreateInboxItemParams) (holdSub, bool, error) {
	if p.RecipientType != "member" || !p.IssueID.Valid || !p.RecipientID.Valid || holdExemptTypes[p.Type] {
		return holdSub{}, false, nil
	}
	workspaceID := util.UUIDToString(p.WorkspaceID)
	subs := h.subscriptions(ctx).forOwner(workspaceID, util.UUIDToString(p.RecipientID))
	if len(subs) == 0 {
		return holdSub{}, false, nil
	}
	chain, projectID, err := issueScope(ctx, h.Store.DB, workspaceID, util.UUIDToString(p.IssueID))
	if err != nil {
		return holdSub{}, false, err
	}
	if len(chain) == 0 {
		// Not visible outside the writer's transaction, or gone.
		h.stats.issueMissing.Add(1)
		return holdSub{}, false, nil
	}
	sub, ok := narrowestHold(subs, chain, projectID)
	if !ok {
		return holdSub{}, false, nil
	}
	if p.ActorType.String == "agent" && util.UUIDToString(p.ActorID) == sub.AgentID {
		return holdSub{}, false, nil
	}
	return sub, true, nil
}

// narrowestHold picks, among one member's subscriptions, the one that covers
// the issue most narrowly (the rule of pickNarrowest, across agents).
func narrowestHold(subs []holdSub, chain []string, projectID string) (holdSub, bool) {
	depth := chainDepths(chain)
	var best holdSub
	bestRank := -1
	for _, s := range subs {
		rank := scopeRank(s.scopeSub, depth, projectID)
		if rank < 0 {
			continue
		}
		if bestRank < 0 || narrower(s.scopeSub, rank, best.scopeSub, bestRank) {
			best, bestRank = s, rank
		}
	}
	return best, bestRank >= 0
}

// issueScope returns the issue followed by its ancestors (nearest first, at
// most MaxScopeDepth parents) and the issue's project, in one query.
func issueScope(ctx context.Context, q Querier, workspaceID, issueID string) ([]string, string, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE up AS (
		    SELECT id, parent_issue_id, project_id, 0 AS depth
		    FROM issue WHERE id = $1::uuid AND workspace_id = $2::uuid
		    UNION ALL
		    SELECT p.id, p.parent_issue_id, NULL::uuid, u.depth + 1
		    FROM up u JOIN issue p ON p.id = u.parent_issue_id
		    WHERE u.depth < $3
		)
		SELECT id::text, COALESCE(project_id::text, '') FROM up ORDER BY depth`, issueID, workspaceID, MaxScopeDepth)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var chain []string
	projectID := ""
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, "", err
		}
		if len(chain) == 0 {
			projectID = project
		}
		chain = append(chain, id)
	}
	return chain, projectID, rows.Err()
}

// recordShadow writes the shadow row on the pool, never in the writer's
// transaction: a failed insert there would abort the writer's work.
// note is the rule verdict, if any; decided_at is set with it.
func (h *HoldShadow) recordShadow(ctx context.Context, sub holdSub, p db.CreateInboxItemParams, note *string, took time.Duration) error {
	item, err := json.Marshal(inboxItemJSON(p))
	if err != nil {
		return err
	}
	_, err = h.Store.DB.Exec(ctx, `
		INSERT INTO router_hold (
		    workspace_id, subscription_id, agent_id, recipient_user_id, inbox_item_id, issue_id,
		    item_type, severity, title, actor_type, actor_id, item, mode, status, decide_micros,
		    decision_note, decided_at
		) VALUES (
		    $1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6::uuid,
		    $7, $8, $9, $10, $11::uuid, $12, $13, $14, $15,
		    $16::text, CASE WHEN $16::text IS NULL THEN NULL ELSE now() END
		)
		ON CONFLICT (inbox_item_id) DO NOTHING`,
		util.UUIDToString(p.WorkspaceID), sub.ID, sub.AgentID, sub.OwnerUserID, util.UUIDToString(p.ID),
		util.UUIDToString(p.IssueID), p.Type, p.Severity, p.Title, util.TextToPtr(p.ActorType),
		util.UUIDToPtr(p.ActorID), item, HoldModeShadow, HoldStatusShadow, took.Microseconds(), note)
	return err
}

// inboxItemJSON is the inbox write as router_hold.item keeps it.
func inboxItemJSON(p db.CreateInboxItemParams) map[string]any {
	var details any
	if len(p.Details) > 0 && json.Valid(p.Details) {
		details = json.RawMessage(p.Details)
	}
	return map[string]any{
		"id": util.UUIDToString(p.ID), "workspace_id": util.UUIDToString(p.WorkspaceID),
		"recipient_type": p.RecipientType, "recipient_id": util.UUIDToString(p.RecipientID),
		"type": p.Type, "severity": p.Severity, "issue_id": util.UUIDToPtr(p.IssueID),
		"title": p.Title, "body": util.TextToPtr(p.Body),
		"actor_type": util.TextToPtr(p.ActorType), "actor_id": util.UUIDToPtr(p.ActorID),
		"details": details,
	}
}

// subscriptions returns the cached hold subscriptions, reloading them the
// way Capturer.subscriptions does.
func (h *HoldShadow) subscriptions(ctx context.Context) *holdSnapshot {
	snap := h.snap.Load()
	gen := subscriptionGeneration.Load()
	if snap != nil && snap.gen == gen && time.Since(snap.loadedAt) < h.TTL {
		return snap
	}
	if !h.reloading.CompareAndSwap(false, true) {
		return snap
	}
	defer h.reloading.Store(false)
	if time.Now().Before(h.retryAt) {
		return snap
	}
	byOwner, err := h.loadSubscriptions(ctx)
	if err != nil {
		h.reloadFailures++
		backoff := min(reloadBackoffBase<<min(h.reloadFailures-1, 5), reloadBackoffMax)
		h.retryAt = time.Now().Add(backoff)
		h.stats.reloadFailures.Add(1)
		h.Logger.Warn("router hold: reload subscriptions failed; using the previous copy", "retry_in", backoff, "error", err)
		return snap
	}
	h.reloadFailures, h.retryAt = 0, time.Time{}
	next := &holdSnapshot{byOwner: byOwner, gen: gen, loadedAt: time.Now()}
	h.snap.Store(next)
	return next
}

func (h *HoldShadow) loadSubscriptions(ctx context.Context) (map[[2]string][]holdSub, error) {
	rows, err := h.Store.DB.Query(ctx, `
		SELECT s.id::text, s.workspace_id::text, s.agent_id::text, s.scope_type, s.scope_id::text,
		       s.event_types, s.created_at, COALESCE(`+subscriptionOwnerSQL+`::text, '')
		FROM router_subscription s
		JOIN router_chat_anchor ca ON ca.subscription_id = s.id
		JOIN chat_session cs ON cs.id = ca.chat_session_id
		WHERE s.enabled AND s.paused_reason IS NULL
		  AND cs.status = 'active' AND cs.agent_id = s.agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string][]holdSub{}
	for rows.Next() {
		var s holdSub
		var ws string
		if err := rows.Scan(&s.ID, &ws, &s.AgentID, &s.ScopeType, &s.ScopeID, &s.EventTypes, &s.CreatedAt, &s.OwnerUserID); err != nil {
			return nil, err
		}
		if s.OwnerUserID == "" {
			continue
		}
		key := [2]string{ws, s.OwnerUserID}
		out[key] = append(out[key], s)
	}
	return out, rows.Err()
}

// logFailure logs the first failure and then every 100th, so a database
// outage does not log once per inbox write.
func (h *HoldShadow) logFailure(p db.CreateInboxItemParams, err error) {
	n := h.stats.errors.Load() + h.stats.timeouts.Load()
	if n == 1 || n%100 == 0 {
		h.Logger.Warn("router hold: interceptor failed; the inbox write went ahead",
			"type", p.Type, "failures_total", n, "error", err)
	}
}

// logRuleFailure logs a failed classification the same way: the row is
// recorded without a verdict and the inbox write is not affected.
func (h *HoldShadow) logRuleFailure(p db.CreateInboxItemParams, err error) {
	n := h.rules.failures.Load()
	if n == 1 || n%100 == 0 {
		h.Logger.Warn("router hold: inbox rules failed; recorded the item without a verdict",
			"type", p.Type, "failures_total", n, "error", err)
	}
}

// holdStats are this process's interceptor counters since it started.
type holdStats struct {
	since          time.Time
	calls          atomic.Int64
	wouldHold      atomic.Int64
	errors         atomic.Int64
	timeouts       atomic.Int64
	panics         atomic.Int64
	issueMissing   atomic.Int64
	reloadFailures atomic.Int64
	totalMicros    atomic.Int64
	maxMicros      atomic.Int64
	buckets        [len(holdLatencyBuckets) + 1]atomic.Int64
}

// holdLatencyBuckets are the upper bounds of the latency histogram; the last
// bucket counts everything slower.
var holdLatencyBuckets = [...]time.Duration{time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond}

var holdLatencyLabels = [...]string{"le_1ms", "le_5ms", "le_20ms", "le_100ms", "gt_100ms"}

func (s *holdStats) observe(took time.Duration, err error, timedOut bool) {
	s.calls.Add(1)
	us := took.Microseconds()
	s.totalMicros.Add(us)
	for cur := s.maxMicros.Load(); us > cur && !s.maxMicros.CompareAndSwap(cur, us); cur = s.maxMicros.Load() {
	}
	i := 0
	for i < len(holdLatencyBuckets) && took > holdLatencyBuckets[i] {
		i++
	}
	s.buckets[i].Add(1)
	switch {
	case err == nil:
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		s.timeouts.Add(1)
	default:
		s.errors.Add(1)
	}
}

// HoldStats is a snapshot of the interceptor's counters in this server
// process since it started. Errors, timeouts and panics are calls where the
// interceptor failed and the inbox write went ahead without it. Hook counts
// what the inbox hook saw: writes that did not wait for an answer past the
// budget, and writes that skipped the interceptor because every call slot
// was taken.
type HoldStats struct {
	Since          time.Time            `json:"since"`
	Calls          int64                `json:"calls"`
	WouldHold      int64                `json:"would_hold"`
	Errors         int64                `json:"errors"`
	Timeouts       int64                `json:"timeouts"`
	Panics         int64                `json:"panics"`
	IssueMissing   int64                `json:"issue_missing"`
	ReloadFailures int64                `json:"reload_failures"`
	AvgMicros      int64                `json:"avg_micros"`
	MaxMicros      int64                `json:"max_micros"`
	Latency        map[string]int64     `json:"latency"`
	Hook           inbox.InterceptStats `json:"hook"`
	// Rules counts the rule verdicts and failures; nil while rule
	// classification is off in this process.
	Rules *InboxRulesStats `json:"rules"`
}

func (h *HoldShadow) Stats() HoldStats {
	s := &h.stats
	out := HoldStats{
		Since: s.since, Calls: s.calls.Load(), WouldHold: s.wouldHold.Load(), Errors: s.errors.Load(),
		Timeouts: s.timeouts.Load(), Panics: s.panics.Load(), IssueMissing: s.issueMissing.Load(),
		ReloadFailures: s.reloadFailures.Load(), MaxMicros: s.maxMicros.Load(), Latency: map[string]int64{},
		Hook: inbox.CurrentInterceptStats(), Rules: h.rules.stats(),
	}
	if out.Calls > 0 {
		out.AvgMicros = s.totalMicros.Load() / out.Calls
	}
	for i, label := range holdLatencyLabels {
		out.Latency[label] = s.buckets[i].Load()
	}
	return out
}

// CurrentHoldStats returns the stats of the interceptor this process
// registered, or false when the hold is off here.
func CurrentHoldStats() (HoldStats, bool) {
	h := activeHold.Load()
	if h == nil {
		return HoldStats{}, false
	}
	return h.Stats(), true
}

// Hold is one router_hold row as read back.
type Hold struct {
	ID              string    `json:"id"`
	SubscriptionID  string    `json:"subscription_id"`
	AgentID         string    `json:"agent_id"`
	RecipientUserID string    `json:"recipient_user_id"`
	InboxItemID     string    `json:"inbox_item_id"`
	InInbox         bool      `json:"in_inbox"`
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	ItemType        string    `json:"item_type"`
	Severity        string    `json:"severity"`
	Title           string    `json:"title"`
	ActorType       *string   `json:"actor_type"`
	ActorID         *string   `json:"actor_id"`
	Mode            string    `json:"mode"`
	Status          string    `json:"status"`
	DecideMicros    int       `json:"decide_micros"`
	DispatchTaskID  *string   `json:"dispatch_task_id"`
	CreatedAt       time.Time `json:"created_at"`
	// Verdict and Rule are the rule verdict (inbox_rules.go); empty when the
	// item was not classified.
	Verdict string `json:"verdict"`
	Rule    string `json:"rule"`
}

// HoldFilter selects hold rows. RecipientUserID and AgentID restrict them to
// one member's items or one agent's subscriptions.
type HoldFilter struct {
	WorkspaceID     string
	SubscriptionID  string
	RecipientUserID string
	AgentID         string
	Limit           int
}

// ListHolds returns hold rows, newest first.
func (s *Store) ListHolds(ctx context.Context, f HoldFilter) ([]Hold, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.Query(ctx, `
		SELECT h.id::text, h.subscription_id::text, h.agent_id::text, h.recipient_user_id::text,
		       h.inbox_item_id::text, EXISTS (SELECT 1 FROM inbox_item ii WHERE ii.id = h.inbox_item_id),
		       h.issue_id::text, COALESCE(w.issue_prefix || '-' || i.number, ''),
		       h.item_type, h.severity, h.title, h.actor_type, h.actor_id::text,
		       h.mode, h.status, h.decide_micros, h.dispatch_task_id::text, h.created_at, COALESCE(h.decision_note, '')
		FROM router_hold h
		LEFT JOIN issue i ON i.id = h.issue_id
		LEFT JOIN workspace w ON w.id = i.workspace_id
		WHERE h.workspace_id = $1::uuid
		  AND ($2 = '' OR h.subscription_id = NULLIF($2, '')::uuid)
		  AND ($3 = '' OR h.recipient_user_id = NULLIF($3, '')::uuid)
		  AND ($4 = '' OR h.agent_id = NULLIF($4, '')::uuid)
		ORDER BY h.created_at DESC, h.id DESC
		LIMIT $5`, f.WorkspaceID, f.SubscriptionID, f.RecipientUserID, f.AgentID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hold, error) {
		var x Hold
		var note string
		err := r.Scan(&x.ID, &x.SubscriptionID, &x.AgentID, &x.RecipientUserID, &x.InboxItemID, &x.InInbox,
			&x.IssueID, &x.IssueIdentifier, &x.ItemType, &x.Severity, &x.Title, &x.ActorType, &x.ActorID,
			&x.Mode, &x.Status, &x.DecideMicros, &x.DispatchTaskID, &x.CreatedAt, &note)
		if v, ok := ParseDecisionNote(note); ok {
			x.Verdict, x.Rule = v.Verdict, v.Rule
		}
		return x, err
	})
}

// shadowHold is a shadow row as a batch lists it.
type shadowHold struct {
	ID        string
	ItemType  string
	Title     string
	IssueKey  string
	ActorType *string
	ActorName string
	InInbox   bool
	CreatedAt time.Time
}

const maxBatchShadowHolds = 30

// loadShadowHolds reads a subscription's shadow rows no batch has listed yet,
// oldest first, and how many there are in all.
func loadShadowHolds(ctx context.Context, q Querier, subscriptionID string) ([]shadowHold, int, error) {
	rows, err := q.Query(ctx, `
		SELECT h.id::text, h.item_type, h.title, COALESCE(w.issue_prefix || '-' || i.number, ''),
		       h.actor_type, COALESCE(a.name, u.name, ''),
		       EXISTS (SELECT 1 FROM inbox_item ii WHERE ii.id = h.inbox_item_id), h.created_at,
		       count(*) OVER ()
		FROM router_hold h
		LEFT JOIN issue i ON i.id = h.issue_id
		LEFT JOIN workspace w ON w.id = i.workspace_id
		LEFT JOIN agent a ON h.actor_type = 'agent' AND a.id = h.actor_id
		LEFT JOIN "user" u ON h.actor_type = 'member' AND u.id = h.actor_id
		WHERE h.subscription_id = $1::uuid AND h.status = 'shadow' AND h.dispatch_task_id IS NULL
		ORDER BY h.created_at, h.id
		LIMIT $2`, subscriptionID, maxBatchShadowHolds)
	if err != nil {
		return nil, 0, err
	}
	total := 0
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (shadowHold, error) {
		var x shadowHold
		err := r.Scan(&x.ID, &x.ItemType, &x.Title, &x.IssueKey, &x.ActorType, &x.ActorName, &x.InInbox, &x.CreatedAt, &total)
		return x, err
	})
	return out, total, err
}

// pruneShadowHolds deletes shadow rows older than holdRetention, a bounded
// number per call.
func pruneShadowHolds(ctx context.Context, q Querier) (int64, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM router_hold WHERE id IN (
		    SELECT id FROM router_hold
		    WHERE status = 'shadow' AND created_at < now() - make_interval(secs => $1)
		    LIMIT 1000)`, holdRetention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
