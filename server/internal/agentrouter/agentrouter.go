// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86).

// Package agentrouter captures issue events for agents that subscribe to an
// issue tree or a project (the "Router" of the fork's Super-agent design), and
// runs the subscribed agent on them.
//
// Slice S1 captures: a subscription, a durable event queue, an in-process
// events.Bus listener and a periodic reconcile that adds whatever the listener
// missed. Slice S2 dispatches: it batches a subscription's pending events,
// starts one anchor run per batch (one run at a time per agent), keeps a small
// state document per subscription, and gives the agent one outlet to reach a
// member, the owner notice, delivered by channel adapters (inbox only).
//
// Capture never uses database triggers. The listener runs synchronously on
// the publisher's goroutine, so it does one bounded lookup and one short
// transaction per event and only for workspaces that hold an enabled
// subscription. Anything it misses (a failed insert, a write path that never
// publishes, a process that dies between commit and publish, a subscription
// cache that has not caught up) is added by reconcile and counted as
// capture_miss.
//
// Capture is behind the router_v1 feature flag: with it off the API answers 404
// and neither the listener nor the reconcile job is registered. Dispatch, state
// and notices are additionally behind router_dispatch.
package agentrouter

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	ScopeIssueTree = "issue_tree"
	ScopeProject   = "project"

	KindIssueCreated    = "issue_created"
	KindStatusChanged   = "status_changed"
	KindAssigneeChanged = "assignee_changed"
	KindCommentCreated  = "comment_created"
	KindTaskFailed      = "task_failed"

	SourceBus       = "bus"
	SourceReconcile = "reconcile"

	PausedManual           = "manual"
	PausedRate             = "rate"
	PausedAgentUnavailable = "agent_unavailable"

	// Batching defaults and bounds, in seconds (debounce, max wait) and runs.
	DefaultDebounceSeconds = 60
	DefaultMaxWaitSeconds  = 600
	DefaultMaxRunsPerHour  = 6
	MinDebounceSeconds     = 10
	MaxBatchWaitSeconds    = 86400
	MaxRunsPerHourLimit    = 60

	// MaxStateBytes caps a subscription's state document.
	MaxStateBytes = 8 * 1024

	// MaxScopeDepth bounds how far an issue tree is followed, in both
	// directions: the listener walks up at most this many parents and
	// reconcile walks down at most this many levels, so both agree on what is
	// in scope. It also stops a parent cycle from recursing forever.
	MaxScopeDepth = 32

	// projectRank orders a project scope after every issue-tree scope when
	// picking an agent's narrowest subscription.
	projectRank = 1000
)

// AllKinds is every event kind S1 captures, and the default for a new
// subscription's event types.
var AllKinds = []string{
	KindIssueCreated,
	KindStatusChanged,
	KindAssigneeChanged,
	KindCommentCreated,
	KindTaskFailed,
}

// NormalizeEventTypes validates requested event kinds and returns them in
// canonical order without duplicates. An empty request means every kind.
func NormalizeEventTypes(in []string) ([]string, error) {
	if len(in) == 0 {
		return slices.Clone(AllKinds), nil
	}
	want := map[string]bool{}
	for _, raw := range in {
		kind := strings.TrimSpace(raw)
		if kind == "" {
			continue
		}
		if !slices.Contains(AllKinds, kind) {
			return nil, fmt.Errorf("unknown event type %q (valid: %s)", kind, strings.Join(AllKinds, ", "))
		}
		want[kind] = true
	}
	if len(want) == 0 {
		return slices.Clone(AllKinds), nil
	}
	out := make([]string, 0, len(want))
	for _, kind := range AllKinds {
		if want[kind] {
			out = append(out, kind)
		}
	}
	return out, nil
}

// Subscription is one agent watching one scope.
type Subscription struct {
	ID            string   `json:"id"`
	WorkspaceID   string   `json:"workspace_id"`
	AgentID       string   `json:"agent_id"`
	OwnerUserID   *string  `json:"owner_user_id"`
	ScopeType     string   `json:"scope_type"`
	ScopeID       string   `json:"scope_id"`
	AnchorIssueID *string  `json:"anchor_issue_id"`
	EventTypes    []string `json:"event_types"`
	Enabled       bool     `json:"enabled"`
	// PausedReason with Enabled false is a manual pause: capture stopped.
	// With Enabled true it is "rate" or "agent_unavailable": dispatch stopped
	// but capture goes on, so a resume dispatches what arrived meanwhile.
	PausedReason    *string   `json:"paused_reason"`
	DebounceSeconds int       `json:"debounce_seconds"`
	MaxWaitSeconds  int       `json:"max_wait_seconds"`
	MaxRunsPerHour  int       `json:"max_runs_per_hour"`
	StateRevision   int64     `json:"state_revision"`
	StateBytes      int       `json:"state_bytes"`
	ActiveSince     time.Time `json:"active_since"`
	// ReconciledThrough is the upper bound of the last reconcile pass that
	// completed for this subscription's workspace. CaptureMiss says nothing
	// about changes after it.
	ReconciledThrough *time.Time `json:"reconciled_through"`
	CreatedByType     string     `json:"created_by_type"`
	CreatedByID       string     `json:"created_by_id"`
	// ViaAgentID and ViaTaskID are set when an agent created the
	// subscription for another agent on the authority of CreatedByID, the
	// member who started that agent's run.
	ViaAgentID *string   `json:"via_agent_id"`
	ViaTaskID  *string   `json:"via_task_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// EventCount and CaptureMiss are computed on read. CaptureMiss is the
	// number of this subscription's events that reconcile had to add because
	// the listener did not record them.
	EventCount  int64 `json:"event_count"`
	CaptureMiss int64 `json:"capture_miss"`
}

// Event is one captured change.
type Event struct {
	ID              string         `json:"id"`
	SubscriptionID  string         `json:"subscription_id"`
	AgentID         string         `json:"agent_id"`
	EventKey        string         `json:"event_key"`
	Kind            string         `json:"kind"`
	IssueID         string         `json:"issue_id"`
	IssueIdentifier string         `json:"issue_identifier"`
	ActorType       *string        `json:"actor_type"`
	ActorID         *string        `json:"actor_id"`
	Source          string         `json:"source"`
	Self            bool           `json:"self"`
	Payload         map[string]any `json:"payload"`
	OccurredAt      time.Time      `json:"occurred_at"`
	ConsumedTaskID  *string        `json:"consumed_task_id"`
	CreatedAt       time.Time      `json:"created_at"`
}

// ScopeIssue is one issue a subscription currently owns after the narrowest
// scope rule.
type ScopeIssue struct {
	ID           string    `json:"id"`
	Identifier   string    `json:"identifier"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	AssigneeType *string   `json:"assignee_type"`
	AssigneeID   *string   `json:"assignee_id"`
	ParentID     *string   `json:"parent_issue_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Batching is a subscription's dispatch parameters. A zero field means "keep"
// on update and "default" on create.
type Batching struct {
	DebounceSeconds int
	MaxWaitSeconds  int
	MaxRunsPerHour  int
}

// Validate checks the non-zero fields against their bounds, and max wait
// against debounce when both are known.
func (b Batching) Validate() error {
	if b.DebounceSeconds != 0 && (b.DebounceSeconds < MinDebounceSeconds || b.DebounceSeconds > MaxBatchWaitSeconds) {
		return fmt.Errorf("debounce_seconds must be between %d and %d", MinDebounceSeconds, MaxBatchWaitSeconds)
	}
	if b.MaxWaitSeconds != 0 && (b.MaxWaitSeconds < MinDebounceSeconds || b.MaxWaitSeconds > MaxBatchWaitSeconds) {
		return fmt.Errorf("max_wait_seconds must be between %d and %d", MinDebounceSeconds, MaxBatchWaitSeconds)
	}
	if b.DebounceSeconds != 0 && b.MaxWaitSeconds != 0 && b.MaxWaitSeconds < b.DebounceSeconds {
		return fmt.Errorf("max_wait_seconds must not be shorter than debounce_seconds")
	}
	if b.MaxRunsPerHour != 0 && (b.MaxRunsPerHour < 1 || b.MaxRunsPerHour > MaxRunsPerHourLimit) {
		return fmt.Errorf("max_runs_per_hour must be between 1 and %d", MaxRunsPerHourLimit)
	}
	return nil
}

// withDefaults fills zero fields with the defaults.
func (b Batching) withDefaults() Batching {
	if b.DebounceSeconds == 0 {
		b.DebounceSeconds = DefaultDebounceSeconds
	}
	if b.MaxWaitSeconds == 0 {
		b.MaxWaitSeconds = max(DefaultMaxWaitSeconds, b.DebounceSeconds)
	}
	if b.MaxRunsPerHour == 0 {
		b.MaxRunsPerHour = DefaultMaxRunsPerHour
	}
	return b
}

func issueCreatedKey(issueID string) string { return "issue:" + issueID + ":created" }

func commentKey(commentID string) string { return "comment:" + commentID }

func taskFailedKey(taskID string) string { return "task:" + taskID + ":failed" }

// levelChangeKey keys a status or assignee change by the issue's updated_at,
// so the listener and reconcile produce the same key for the same row
// version and a later change gets a new one.
func levelChangeKey(issueID, kind string, updatedAt time.Time) string {
	return "issue:" + issueID + ":" + kind + ":" + updatedAt.UTC().Format("20060102T150405.000000Z")
}

func isSelf(actorType, actorID *string, agentID string) bool {
	return actorType != nil && actorID != nil && *actorType == "agent" && *actorID == agentID
}
