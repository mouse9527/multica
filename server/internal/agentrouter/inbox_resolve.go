// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package agentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Inbox resolve keeps a workspace owner's inbox in step with the work that
// waits for them (S3a of the M-117 design). An issue waits for the owner while
// it sits in one of the configured waiting statuses, is assigned to them and
// open, or is open with its latest task failed. When an issue stops waiting —
// it leaves a waiting status, is no longer assigned to the owner, or is
// closed while assigned to them — the owner's unarchived inbox items on it
// are archived. It never holds back, delays or writes an inbox item.
//
// Two paths do the archiving. An events.Bus listener acts on issue:updated
// right after the change. A periodic reconcile archives what the listener
// missed: an owner item on an issue that no longer waits but waited at some
// point after the item was written (activity_log is the record of that), and
// counts the waiting issues that have no owner item without writing one.
//
// It is behind the router_inbox_resolve feature flag (with router_v1), and
// only acts in workspaces whose settings name an owner under
// WorkspaceSettingInboxResolve. With either missing nothing is registered or
// archived, as upstream.

const (
	// FlagInboxResolve is the feature flag (FF_ROUTER_INBOX_RESOLVE). It is
	// declared here rather than in internal/featureflags so the upstream
	// file stays untouched; it only takes effect with router_v1 on.
	FlagInboxResolve = "router_inbox_resolve"
	// WorkspaceSettingInboxResolve is the workspace settings key holding an
	// InboxResolveConfig. Absent or without an owner, the workspace is off.
	WorkspaceSettingInboxResolve = "router_inbox_resolve"
	// JobNameInboxResolve is the scheduler's audit key for the reconcile.
	JobNameInboxResolve = "router_inbox_resolve"

	ArchiveReasonLeftStatus = "left_status"
	ArchiveReasonUnassigned = "unassigned"
	ArchiveReasonReconcile  = "reconcile"

	MissingReasonWaitingStatus = "waiting_status"
	MissingReasonAssigned      = "assigned"
	MissingReasonFailed        = "failed"

	// inboxBatchArchivedReason tells clients why their items were archived.
	inboxBatchArchivedReason = "router_inbox_resolve"

	// DefaultInboxLeaveSlack is how long after an issue left the wait an
	// owner item still counts as written before it. The change that takes an
	// issue out of a waiting status notifies its subscribers on the same
	// publish, a moment after the activity row; that item is about the wait
	// ending and goes with it.
	DefaultInboxLeaveSlack = 10 * time.Second
	// DefaultInboxResolveGrace keeps reconcile away from leaves so recent the
	// listener may still be archiving them, so a race is not counted as a
	// miss.
	DefaultInboxResolveGrace = 30 * time.Second
	// inboxConfigTTL is how long the listener trusts a workspace's settings.
	inboxConfigTTL = 30 * time.Second
	// maxWaitingStatuses bounds the configured status list.
	maxWaitingStatuses = 16
)

// InboxResolveConfig is a workspace's inbox resolve setting.
type InboxResolveConfig struct {
	OwnerUserID     string   `json:"owner_user_id"`
	WaitingStatuses []string `json:"waiting_statuses"`
}

// Active reports whether the workspace has an owner to resolve for.
func (c InboxResolveConfig) Active() bool { return c.OwnerUserID != "" }

// Normalize trims and lowercases the statuses and drops duplicates and blanks.
// An owner that is not a UUID makes the config invalid.
func (c InboxResolveConfig) Normalize() (InboxResolveConfig, error) {
	out := InboxResolveConfig{OwnerUserID: strings.TrimSpace(c.OwnerUserID), WaitingStatuses: []string{}}
	if out.OwnerUserID != "" {
		id, err := uuid.Parse(out.OwnerUserID)
		if err != nil {
			return InboxResolveConfig{}, fmt.Errorf("owner_user_id must be a UUID")
		}
		out.OwnerUserID = id.String()
	}
	for _, s := range c.WaitingStatuses {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !slices.Contains(out.WaitingStatuses, s) {
			out.WaitingStatuses = append(out.WaitingStatuses, s)
		}
	}
	if len(out.WaitingStatuses) > maxWaitingStatuses {
		return InboxResolveConfig{}, fmt.Errorf("at most %d waiting statuses", maxWaitingStatuses)
	}
	return out, nil
}

// ParseInboxResolveConfig reads the setting out of a workspace's settings.
// Anything unreadable or invalid is the zero, inactive config.
func ParseInboxResolveConfig(settings []byte) InboxResolveConfig {
	var values map[string]json.RawMessage
	if json.Unmarshal(settings, &values) != nil {
		return InboxResolveConfig{}
	}
	var c InboxResolveConfig
	if json.Unmarshal(values[WorkspaceSettingInboxResolve], &c) != nil {
		return InboxResolveConfig{}
	}
	out, err := c.Normalize()
	if err != nil {
		return InboxResolveConfig{}
	}
	return out
}

// inboxConfigGeneration moves when this process changes a workspace's
// setting, so the listener drops its cached copy at once.
var inboxConfigGeneration atomic.Int64

// LoadInboxResolveConfig reads a workspace's setting.
func LoadInboxResolveConfig(ctx context.Context, q Querier, workspaceID string) (InboxResolveConfig, error) {
	var settings []byte
	err := q.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, workspaceID).Scan(&settings)
	if errors.Is(err, pgx.ErrNoRows) {
		return InboxResolveConfig{}, ErrNotFound
	}
	if err != nil {
		return InboxResolveConfig{}, err
	}
	return ParseInboxResolveConfig(settings), nil
}

// SetInboxResolveConfig stores a workspace's setting; nil removes it. The
// caller checks that the owner is a member and the statuses exist.
func SetInboxResolveConfig(ctx context.Context, q Querier, workspaceID string, cfg *InboxResolveConfig) error {
	var value any
	if cfg != nil {
		value = cfg
	}
	patch, err := json.Marshal(map[string]any{WorkspaceSettingInboxResolve: value})
	if err != nil {
		return err
	}
	wsID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return ErrInvalid
	}
	if err := db.New(q).MergeWorkspaceSettings(ctx, db.MergeWorkspaceSettingsParams{ID: wsID, Patch: patch}); err != nil {
		return err
	}
	inboxConfigGeneration.Add(1)
	return nil
}

// SQL fragments over issue i with $2 the owner and $3 the waiting statuses.
// They are constants; nothing here comes from input.
const (
	inboxClosedSQL = `(i.status IN ('done', 'cancelled') OR EXISTS (
	    SELECT 1 FROM issue_status st
	    WHERE st.workspace_id = i.workspace_id AND st.key = i.status AND st.category IN ('done', 'closed')))`
	inboxStatusSQL   = `i.status = ANY($3::text[])`
	inboxAssignedSQL = `COALESCE(i.assignee_type = 'member' AND i.assignee_id = $2::uuid, false)`
	inboxFailedSQL   = `EXISTS (
	    SELECT 1 FROM (
	        SELECT t.status FROM agent_task_queue t WHERE t.issue_id = i.id
	        ORDER BY t.created_at DESC, t.id DESC LIMIT 1) last_task
	    WHERE last_task.status = 'failed')`
	// inboxWaitingSQL is "the issue waits for the owner".
	inboxWaitingSQL = `(` + inboxStatusSQL + ` OR (NOT ` + inboxClosedSQL + ` AND (` + inboxAssignedSQL + ` OR ` + inboxFailedSQL + `)))`
	// The reconcile asks whether an issue waited for the owner at some point
	// after an inbox item arrived. Waiting is a state, and activity_log records
	// the changes to it: status rows (status_changed, which a workflow handoff
	// also writes, and duplicate_marked / duplicate_unmarked, which stand in
	// for it when a duplicate mark moves the status) and assignment rows
	// (assignee_changed, and a handoff's status row, which carries the new
	// assignee as handoff_to_* and no assignee row). Every state the issue was
	// in after the item is the state just before one of the change rows that
	// follow it, or the current state; so the issue left a wait after the item
	// if it does not wait now and waited just before one of those rows.
	inboxStatusRowSQL     = `p.action IN ('status_changed', 'duplicate_marked', 'duplicate_unmarked')`
	inboxAssignmentRowSQL = `(p.action = 'assignee_changed' OR (p.action = 'status_changed' AND p.details ? 'handoff_to_type'))`
	inboxBeforeASQL       = `p.issue_id = a.issue_id AND (p.created_at, p.id) < (a.created_at, a.id)`
	inboxFromASQL         = `p.issue_id = a.issue_id AND (p.created_at, p.id) >= (a.created_at, a.id)`
	// inboxStateBeforeSQL joins, for change row a: sb, the status the last
	// status row before a set; sa, the first status row from a on, whose
	// "from" is the status before a when nothing earlier set it; ab, the
	// assignee the last assignment row before a set; aa, the first assignment
	// row from a on.
	inboxStateBeforeSQL = `
	    LEFT JOIN LATERAL (
	        SELECT p.details->>'to' AS status FROM activity_log p
	        WHERE ` + inboxBeforeASQL + ` AND ` + inboxStatusRowSQL + ` AND p.details ? 'to'
	        ORDER BY p.created_at DESC, p.id DESC LIMIT 1) sb ON true
	    LEFT JOIN LATERAL (
	        SELECT p.action, p.details->>'from' AS status FROM activity_log p
	        WHERE ` + inboxFromASQL + ` AND ` + inboxStatusRowSQL + `
	        ORDER BY p.created_at, p.id LIMIT 1) sa ON true
	    LEFT JOIN LATERAL (
	        SELECT p.action,
	               CASE WHEN p.action = 'assignee_changed' THEN p.details->>'to_type' ELSE p.details->>'handoff_to_type' END AS type,
	               CASE WHEN p.action = 'assignee_changed' THEN p.details->>'to_id' ELSE p.details->>'handoff_to_id' END AS id
	        FROM activity_log p
	        WHERE ` + inboxBeforeASQL + ` AND ` + inboxAssignmentRowSQL + `
	        ORDER BY p.created_at DESC, p.id DESC LIMIT 1) ab ON true
	    LEFT JOIN LATERAL (
	        SELECT p.action, p.action = 'assignee_changed' OR p.details ? 'handoff_from_type' AS names_from,
	               CASE WHEN p.details ? 'router_from_type' THEN NULLIF(p.details->>'router_from_type', '')
	                    WHEN p.action = 'assignee_changed' THEN p.details->>'from_type'
	                    ELSE NULLIF(p.details->>'handoff_from_type', '') END AS type,
	               CASE WHEN p.details ? 'router_from_type' THEN NULLIF(p.details->>'router_from_id', '')
	                    WHEN p.action = 'assignee_changed' THEN p.details->>'from_id'
	                    ELSE NULLIF(p.details->>'handoff_from_id', '') END AS id
	        FROM activity_log p
	        WHERE ` + inboxFromASQL + ` AND ` + inboxAssignmentRowSQL + `
	        ORDER BY p.created_at, p.id LIMIT 1) aa ON true`
	// inboxStatusBeforeSQL is the status just before a; NULL when only a
	// duplicate mark row, which names no "from", could tell.
	inboxStatusBeforeSQL = `(CASE WHEN sb.status IS NOT NULL THEN sb.status
	        WHEN sa.action IS NULL THEN i.status
	        WHEN sa.action = 'status_changed' THEN sa.status END)`
	// inboxOwnerBeforeSQL is "the owner was the assignee just before a". When
	// nothing earlier set an assignee, the first assignment row from a on
	// names it: an assignee change in router_from_* (AnnotateAssigneeActivity)
	// or else from_*, a handoff in handoff_from_* (AnnotateHandoffActivity). A handoff row written without them, before
	// inbox resolve ran, names none; the issue was then created with that
	// assignee, and an issue_assigned item for the owner from before a shows
	// it was the owner.
	inboxOwnerBeforeSQL = `(CASE WHEN ab.action IS NOT NULL
	            THEN ab.type IS NOT DISTINCT FROM 'member' AND ab.id IS NOT DISTINCT FROM $2::uuid::text
	        WHEN aa.action IS NULL THEN ` + inboxAssignedSQL + `
	        WHEN aa.names_from
	            THEN aa.type IS NOT DISTINCT FROM 'member' AND aa.id IS NOT DISTINCT FROM $2::uuid::text
	        ELSE EXISTS (
	            SELECT 1 FROM inbox_item q
	            WHERE q.workspace_id = a.workspace_id AND q.issue_id = a.issue_id
	              AND q.recipient_type = 'member' AND q.recipient_id = $2::uuid
	              AND q.type = 'issue_assigned' AND q.created_at <= a.created_at) END)`
	// inboxClosedBeforeSQL is "the status just before a is done or closed".
	inboxClosedBeforeSQL = `(` + inboxStatusBeforeSQL + ` IN ('done', 'cancelled') OR EXISTS (
	    SELECT 1 FROM issue_status st
	    WHERE st.workspace_id = i.workspace_id AND st.key = ` + inboxStatusBeforeSQL + ` AND st.category IN ('done', 'closed')))`
	// inboxWaitedBeforeSQL is "the issue waited for the owner just before a".
	// A failed task is not traced back: it only keeps items, never archives.
	inboxWaitedBeforeSQL = `COALESCE(` + inboxStatusBeforeSQL + ` = ANY($3::text[])
	    OR (` + inboxOwnerBeforeSQL + ` AND NOT COALESCE(` + inboxClosedBeforeSQL + `, false)), false)`
	// inboxLeftAfterItemSQL is "issue i waited for the owner after inbox item
	// ii was written" (it does not wait now; inboxOwnerItemsSQL checks), with
	// $4 the slack and $5 the grace in seconds. Change rows younger than the
	// grace are left to the listener.
	inboxLeftAfterItemSQL = `EXISTS (
	    SELECT 1 FROM activity_log a` + inboxStateBeforeSQL + `
	    WHERE a.issue_id = i.id
	      AND a.action IN ('status_changed', 'assignee_changed', 'duplicate_marked', 'duplicate_unmarked')
	      AND a.created_at >= ii.created_at - make_interval(secs => $4)
	      AND a.created_at < now() - make_interval(secs => $5)
	      AND ` + inboxWaitedBeforeSQL + `)`
	// inboxOwnerItemsSQL selects the owner's unarchived items on issues that
	// no longer wait.
	inboxOwnerItemsSQL = `FROM inbox_item ii
	JOIN issue i ON i.id = ii.issue_id AND i.workspace_id = ii.workspace_id
	WHERE ii.workspace_id = $1 AND ii.recipient_type = 'member' AND ii.recipient_id = $2::uuid
	  AND NOT ii.archived
	  AND NOT ` + inboxWaitingSQL
)

// InboxResolver is the listener and the reconcile.
type InboxResolver struct {
	DB      Querier
	Bus     *events.Bus
	Timeout time.Duration
	Slack   time.Duration
	Grace   time.Duration
	Logger  *slog.Logger

	mu      sync.Mutex
	configs map[string]inboxConfigEntry

	stats inboxResolveStats
}

type inboxConfigEntry struct {
	cfg      InboxResolveConfig
	gen      int64
	loadedAt time.Time
}

func NewInboxResolver(q Querier, bus *events.Bus) *InboxResolver {
	r := &InboxResolver{
		DB: q, Bus: bus, Timeout: DefaultCaptureTimeout, Slack: DefaultInboxLeaveSlack,
		Grace: DefaultInboxResolveGrace, Logger: slog.Default(), configs: map[string]inboxConfigEntry{},
	}
	r.stats.since = time.Now()
	r.stats.archived = map[string]*atomic.Int64{
		ArchiveReasonLeftStatus: {}, ArchiveReasonUnassigned: {}, ArchiveReasonReconcile: {},
	}
	return r
}

// activeInboxResolver is the resolver this process registered, for the API.
var activeInboxResolver atomic.Pointer[InboxResolver]

// Payload keys AnnotateBatchUpdate adds to the batch update's issue:updated
// payload. Upstream's batch payload has no prev_assignee_*; adding those keys
// would also change upstream's notifications (an "unassigned" item for the
// previous member assignee), so the previous assignee rides under the fork's
// own keys, read only by the fork.
const (
	PayloadPrevAssigneeType = "router_prev_assignee_type"
	PayloadPrevAssigneeID   = "router_prev_assignee_id"
)

// activityAnnotation is on while this process runs inbox resolve.
var activityAnnotation atomic.Bool

// SetActivityAnnotation turns AnnotateBatchUpdate, AnnotateHandoffActivity
// and AnnotateAssigneeActivity on or off. Register turns them on with inbox
// resolve; off, they do nothing and payloads and activity rows are exactly
// upstream's.
func SetActivityAnnotation(on bool) { activityAnnotation.Store(on) }

// AnnotateBatchUpdate adds the issue's assignee before a batch update to the
// update's issue:updated payload, under PayloadPrevAssigneeType and
// PayloadPrevAssigneeID (nil for none). The upstream batch update handler
// calls it.
func AnnotateBatchUpdate(payload map[string]any, prevType, prevID *string) {
	if !activityAnnotation.Load() {
		return
	}
	payload[PayloadPrevAssigneeType] = prevType
	payload[PayloadPrevAssigneeID] = prevID
}

// payloadPrevAssignee is the assignee before the update an issue:updated
// payload carries: upstream's prev_assignee_* (single updates), else the
// fork's keys (batch updates). ok is false when the payload carries neither,
// so callers write nothing rather than claim there was no assignee.
func payloadPrevAssignee(payload map[string]any) (typ, id string, ok bool) {
	deref := func(v any) string {
		if s, isPtr := v.(*string); isPtr && s != nil {
			return *s
		}
		return ""
	}
	for _, keys := range [][2]string{{"prev_assignee_type", "prev_assignee_id"}, {PayloadPrevAssigneeType, PayloadPrevAssigneeID}} {
		if _, has := payload[keys[0]]; has {
			return deref(payload[keys[0]]), deref(payload[keys[1]]), true
		}
	}
	return "", "", false
}

// AnnotateHandoffActivity adds the previous assignee, as handoff_from_type
// and handoff_from_id ("" for none), to the details of the status activity
// row a workflow handoff writes. That row replaces the assignee row, which
// would have named the previous assignee, so without these reconcile cannot
// tell that a handoff took an issue away from the owner. The upstream
// activity listener calls it for handoff rows. It writes nothing when the
// payload does not say who the assignee was, which leaves reconcile its
// fallback for such rows.
func AnnotateHandoffActivity(details map[string]string, payload map[string]any) {
	if !activityAnnotation.Load() {
		return
	}
	if typ, id, ok := payloadPrevAssignee(payload); ok {
		details["handoff_from_type"], details["handoff_from_id"] = typ, id
	}
}

// AnnotateAssigneeActivity adds the previous assignee, as router_from_type
// and router_from_id ("" for none), to an assignee_changed activity row.
// Upstream fills from_* only from prev_assignee_*, which a batch update does
// not send, so its row reads as "from no one". The fork's keys leave from_*,
// which the timeline shows, as upstream writes it. The upstream activity
// listener calls it for assignee rows.
func AnnotateAssigneeActivity(details map[string]string, payload map[string]any) {
	if !activityAnnotation.Load() {
		return
	}
	if typ, id, ok := payloadPrevAssignee(payload); ok {
		details["router_from_type"], details["router_from_id"] = typ, id
	}
}

// Subscribe registers the listener.
func (r *InboxResolver) Subscribe(bus *events.Bus) {
	bus.Subscribe(protocol.EventIssueUpdated, r.Handle)
}

// issueUpdate is what the listener reads from an issue:updated payload.
type issueUpdate struct {
	Issue struct {
		ID           string  `json:"id"`
		Status       string  `json:"status"`
		AssigneeType *string `json:"assignee_type"`
		AssigneeID   *string `json:"assignee_id"`
	} `json:"issue"`
	StatusChanged    bool    `json:"status_changed"`
	PrevStatus       string  `json:"prev_status"`
	AssigneeChanged  bool    `json:"assignee_changed"`
	PrevAssigneeType *string `json:"prev_assignee_type"`
	PrevAssigneeID   *string `json:"prev_assignee_id"`
	// RouterPrevAssignee* are AnnotateBatchUpdate's keys, set when the
	// payload has no prev_assignee_*.
	RouterPrevAssigneeType *string `json:"router_prev_assignee_type"`
	RouterPrevAssigneeID   *string `json:"router_prev_assignee_id"`
}

// leaveReason says whether the update may have ended the issue's wait for
// the owner as far as the payload shows, and how: it moved out of a waiting
// status, moved status while assigned to the owner (closing it ends that
// wait), or moved away from the owner as assignee. The database decides
// whether the issue still waits.
func (u issueUpdate) leaveReason(cfg InboxResolveConfig) string {
	isOwner := u.Issue.AssigneeType != nil && *u.Issue.AssigneeType == "member" &&
		u.Issue.AssigneeID != nil && *u.Issue.AssigneeID == cfg.OwnerUserID
	if u.StatusChanged && !slices.Contains(cfg.WaitingStatuses, u.Issue.Status) &&
		(slices.Contains(cfg.WaitingStatuses, u.PrevStatus) || isOwner) {
		return ArchiveReasonLeftStatus
	}
	wasOwner := u.PrevAssigneeType != nil && *u.PrevAssigneeType == "member" &&
		u.PrevAssigneeID != nil && *u.PrevAssigneeID == cfg.OwnerUserID
	if u.AssigneeChanged && wasOwner && !isOwner {
		return ArchiveReasonUnassigned
	}
	return ""
}

// Handle acts on one issue:updated. It runs after the change is committed,
// on the publisher's goroutine, and never fails the publisher: an error is
// logged and counted, and reconcile archives what it left.
func (r *InboxResolver) Handle(e events.Event) {
	if e.WorkspaceID == "" {
		return
	}
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return
	}
	var u issueUpdate
	if json.Unmarshal(raw, &u) != nil || u.Issue.ID == "" || (!u.StatusChanged && !u.AssigneeChanged) {
		return
	}
	if u.PrevAssigneeType == nil && u.PrevAssigneeID == nil {
		u.PrevAssigneeType, u.PrevAssigneeID = u.RouterPrevAssigneeType, u.RouterPrevAssigneeID
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.Timeout)
	defer cancel()
	cfg, err := r.config(ctx, e.WorkspaceID)
	if err != nil {
		r.fail(e.WorkspaceID, u.Issue.ID, err)
		return
	}
	if !cfg.Active() {
		return
	}
	reason := u.leaveReason(cfg)
	if reason == "" {
		return
	}
	r.stats.leaves.Add(1)
	if _, err := r.archiveIfDone(ctx, e.WorkspaceID, u.Issue.ID, cfg, reason); err != nil {
		r.fail(e.WorkspaceID, u.Issue.ID, err)
	}
}

func (r *InboxResolver) fail(workspaceID, issueID string, err error) {
	n := r.stats.failures.Add(1)
	r.Logger.Warn("router: inbox resolve failed; reconcile will archive what is left",
		"workspace_id", workspaceID, "issue_id", issueID, "failures_total", n, "error", err)
}

// archiveIfDone archives the owner's items on the issue unless it still waits
// for them. It returns the number of rows archived.
func (r *InboxResolver) archiveIfDone(ctx context.Context, workspaceID, issueID string, cfg InboxResolveConfig, reason string) (int64, error) {
	var waiting bool
	err := r.DB.QueryRow(ctx, `SELECT `+inboxWaitingSQL+` FROM issue i WHERE i.workspace_id = $1 AND i.id = $4`,
		workspaceID, cfg.OwnerUserID, cfg.WaitingStatuses, issueID).Scan(&waiting)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if waiting {
		r.stats.stillWaiting.Add(1)
		return 0, nil
	}
	wsID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return 0, err
	}
	issue, err := util.ParseUUID(issueID)
	if err != nil {
		return 0, err
	}
	owner, err := util.ParseUUID(cfg.OwnerUserID)
	if err != nil {
		return 0, err
	}
	n, err := db.New(r.DB).ArchiveInboxByIssue(ctx, db.ArchiveInboxByIssueParams{
		WorkspaceID: wsID, RecipientType: "member", RecipientID: owner, IssueID: issue,
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		r.stats.archived[reason].Add(n)
		r.publishArchived(workspaceID, cfg.OwnerUserID, issueID, n)
		r.Logger.Info("router: archived the owner's inbox items on an issue that stopped waiting",
			"workspace_id", workspaceID, "issue_id", issueID, "reason", reason, "rows", n)
	}
	return n, nil
}

// publishArchived tells the owner's clients to drop the archived rows, as the
// upstream task_failed auto-archive does.
func (r *InboxResolver) publishArchived(workspaceID, ownerID, issueID string, n int64) {
	if r.Bus == nil {
		return
	}
	r.Bus.Publish(events.Event{
		Type:        protocol.EventInboxBatchArchived,
		WorkspaceID: workspaceID,
		Payload: map[string]any{
			"recipient_id": ownerID,
			"count":        n,
			"issue_id":     issueID,
			"reason":       inboxBatchArchivedReason,
		},
	})
}

// config returns the workspace's setting from the cache, reloading it when
// stale or changed in this process.
func (r *InboxResolver) config(ctx context.Context, workspaceID string) (InboxResolveConfig, error) {
	gen := inboxConfigGeneration.Load()
	r.mu.Lock()
	entry, ok := r.configs[workspaceID]
	r.mu.Unlock()
	if ok && entry.gen == gen && time.Since(entry.loadedAt) < inboxConfigTTL {
		return entry.cfg, nil
	}
	cfg, err := LoadInboxResolveConfig(ctx, r.DB, workspaceID)
	if errors.Is(err, ErrNotFound) {
		cfg, err = InboxResolveConfig{}, nil
	}
	if err != nil {
		return InboxResolveConfig{}, err
	}
	r.mu.Lock()
	r.configs[workspaceID] = inboxConfigEntry{cfg: cfg, gen: gen, loadedAt: time.Now()}
	r.mu.Unlock()
	return cfg, nil
}

// InboxReconcileResult reports one reconcile pass.
type InboxReconcileResult struct {
	At         time.Time `json:"at"`
	Workspaces int       `json:"workspaces"`
	// Archived is the owner rows this pass archived: excess the listener
	// missed. ArchivedIssues is the issues they were on.
	Archived       int64 `json:"archived"`
	ArchivedIssues int   `json:"archived_issues"`
	// Missing counts waiting issues without an unarchived owner item, by why
	// they wait. Nothing is written for them.
	Missing map[string]int64 `json:"missing"`
	// OutsideWait counts the owner's unarchived items on issues that do not
	// wait for them and never left a wait after the item arrived. S3a leaves
	// them alone; they are what later slices sort.
	OutsideWait int64 `json:"outside_wait"`
}

// MissingTotal is the pass's missing count.
func (r InboxReconcileResult) MissingTotal() int64 {
	var n int64
	for _, v := range r.Missing {
		n += v
	}
	return n
}

// Reconcile runs one pass over every workspace with an active setting. A
// failing workspace is logged and skipped; the error joins them all.
// Archiving is conditional on the row still being unarchived, so passes on
// several replicas, or a pass racing the listener, archive and count each
// row once.
func (r *InboxResolver) Reconcile(ctx context.Context) (InboxReconcileResult, error) {
	res := InboxReconcileResult{At: time.Now(), Missing: map[string]int64{}}
	rows, err := r.DB.Query(ctx, `SELECT id::text, settings FROM workspace WHERE settings ? $1`, WorkspaceSettingInboxResolve)
	if err != nil {
		return res, err
	}
	type wsConfig struct {
		id  string
		cfg InboxResolveConfig
	}
	var todo []wsConfig
	for rows.Next() {
		var id string
		var settings []byte
		if err := rows.Scan(&id, &settings); err != nil {
			rows.Close()
			return res, err
		}
		if cfg := ParseInboxResolveConfig(settings); cfg.Active() {
			todo = append(todo, wsConfig{id, cfg})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	var errs []error
	for _, w := range todo {
		if err := r.reconcileWorkspace(ctx, w.id, w.cfg, &res); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", w.id, err))
			r.Logger.Warn("router: inbox resolve reconcile failed", "workspace_id", w.id, "error", err)
			continue
		}
		res.Workspaces++
	}
	r.stats.recordPass(res)
	if res.Archived > 0 || res.MissingTotal() > 0 {
		r.Logger.Info("router: inbox resolve reconcile",
			"archived", res.Archived, "issues", res.ArchivedIssues, "missing", res.Missing, "outside_wait", res.OutsideWait)
	}
	return res, errors.Join(errs...)
}

// ReconcileWorkspace runs one pass over one workspace, for tests and repair.
func (r *InboxResolver) ReconcileWorkspace(ctx context.Context, workspaceID string) (InboxReconcileResult, error) {
	res := InboxReconcileResult{At: time.Now(), Missing: map[string]int64{}}
	cfg, err := LoadInboxResolveConfig(ctx, r.DB, workspaceID)
	if err != nil || !cfg.Active() {
		return res, err
	}
	res.Workspaces = 1
	err = r.reconcileWorkspace(ctx, workspaceID, cfg, &res)
	r.stats.recordPass(res)
	return res, err
}

func (r *InboxResolver) reconcileWorkspace(ctx context.Context, workspaceID string, cfg InboxResolveConfig, res *InboxReconcileResult) error {
	args := []any{workspaceID, cfg.OwnerUserID, cfg.WaitingStatuses, r.Slack.Seconds(), r.Grace.Seconds()}
	rows, err := r.DB.Query(ctx, `
		WITH excess AS (
		    SELECT ii.id `+inboxOwnerItemsSQL+` AND `+inboxLeftAfterItemSQL+`
		)
		UPDATE inbox_item u SET archived = true
		FROM excess WHERE u.id = excess.id AND NOT u.archived
		RETURNING u.issue_id::text`, args...)
	if err != nil {
		return err
	}
	byIssue := map[string]int64{}
	for rows.Next() {
		var issueID string
		if err := rows.Scan(&issueID); err != nil {
			rows.Close()
			return err
		}
		byIssue[issueID]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for issueID, n := range byIssue {
		res.Archived += n
		res.ArchivedIssues++
		r.stats.archived[ArchiveReasonReconcile].Add(n)
		r.publishArchived(workspaceID, cfg.OwnerUserID, issueID, n)
	}
	survey, err := SurveyInbox(ctx, r.DB, workspaceID, cfg)
	if err != nil {
		return err
	}
	for k, v := range survey.Missing {
		res.Missing[k] += v
	}
	res.OutsideWait += survey.OutsideWait
	return nil
}

// InboxSurvey is a read-only look at a workspace's owner inbox against the
// issues that wait for the owner.
type InboxSurvey struct {
	// Excess is the owner's unarchived items on issues that left the wait
	// after the item arrived: what reconcile archives.
	Excess      int64            `json:"excess"`
	OutsideWait int64            `json:"outside_wait"`
	Missing     map[string]int64 `json:"missing"`
}

// SurveyInbox counts excess, outside-the-wait and missing items without
// changing anything.
func SurveyInbox(ctx context.Context, q Querier, workspaceID string, cfg InboxResolveConfig) (InboxSurvey, error) {
	out := InboxSurvey{Missing: map[string]int64{}}
	args := []any{workspaceID, cfg.OwnerUserID, cfg.WaitingStatuses, DefaultInboxLeaveSlack.Seconds(), 0.0}
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE s.left_wait), count(*) FILTER (WHERE NOT s.left_wait)
		FROM (SELECT `+inboxLeftAfterItemSQL+` AS left_wait `+inboxOwnerItemsSQL+`) s`, args...).
		Scan(&out.Excess, &out.OutsideWait)
	if err != nil {
		return out, err
	}
	rows, err := q.Query(ctx, `
		SELECT reason, count(*) FROM (
		    SELECT CASE WHEN `+inboxStatusSQL+` THEN '`+MissingReasonWaitingStatus+`'
		                WHEN `+inboxAssignedSQL+` THEN '`+MissingReasonAssigned+`'
		                ELSE '`+MissingReasonFailed+`' END AS reason
		    FROM issue i
		    WHERE i.workspace_id = $1 AND `+inboxWaitingSQL+`
		      AND NOT EXISTS (
		          SELECT 1 FROM inbox_item ii
		          WHERE ii.workspace_id = $1 AND ii.recipient_type = 'member' AND ii.recipient_id = $2::uuid
		            AND ii.issue_id = i.id AND NOT ii.archived)
		) m GROUP BY reason`, workspaceID, cfg.OwnerUserID, cfg.WaitingStatuses)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var n int64
		if err := rows.Scan(&reason, &n); err != nil {
			return out, err
		}
		out.Missing[reason] = n
	}
	return out, rows.Err()
}

// inboxResolveStats are this process's counters since it started.
type inboxResolveStats struct {
	since        time.Time
	leaves       atomic.Int64
	stillWaiting atomic.Int64
	failures     atomic.Int64
	archived     map[string]*atomic.Int64
	passes       atomic.Int64
	last         atomic.Pointer[InboxReconcileResult]
}

func (s *inboxResolveStats) recordPass(res InboxReconcileResult) {
	s.passes.Add(1)
	s.last.Store(&res)
}

// InboxResolveStats is a snapshot of the counters in the server process that
// answered. Reconcile runs on one replica per pass, so LastReconcile is that
// replica's; the scheduler's run history keeps every pass's result.
type InboxResolveStats struct {
	Since time.Time `json:"since"`
	// Leaves is the updates that took an issue out of the wait per their
	// payload; StillWaiting is those the issue still waited after (for
	// another reason), so nothing was archived.
	Leaves       int64 `json:"leaves"`
	StillWaiting int64 `json:"still_waiting"`
	// Failures is listener errors and timeouts.
	Failures        int64                 `json:"failures"`
	Archived        map[string]int64      `json:"archived"`
	ReconcilePasses int64                 `json:"reconcile_passes"`
	LastReconcile   *InboxReconcileResult `json:"last_reconcile"`
}

func (r *InboxResolver) Stats() InboxResolveStats {
	s := &r.stats
	out := InboxResolveStats{
		Since: s.since, Leaves: s.leaves.Load(), StillWaiting: s.stillWaiting.Load(),
		Failures: s.failures.Load(), Archived: map[string]int64{}, ReconcilePasses: s.passes.Load(),
		LastReconcile: s.last.Load(),
	}
	for k, v := range s.archived {
		out.Archived[k] = v.Load()
	}
	return out
}

// CurrentInboxResolveStats returns the stats of the resolver this process
// registered, if any.
func CurrentInboxResolveStats() (InboxResolveStats, bool) {
	r := activeInboxResolver.Load()
	if r == nil {
		return InboxResolveStats{}, false
	}
	return r.Stats(), true
}
