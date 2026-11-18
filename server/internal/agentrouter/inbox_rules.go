// Modified for the mouse9527/multica fork: Router inbox S3b (M-133).

package agentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Inbox rules sort the owner's inbox items by rule, in shadow mode only (S3b
// of the M-117 design). For every item the shadow hold records, the rule list
// in the workspace's settings decides one of three verdicts: the rules handle
// it (it would not need the owner), it goes to the owner, or it is gray (the
// Router would judge it, in a later slice). The verdict and the rule's name go
// on the item's router_hold row as decision_note; the item itself reaches the
// inbox exactly as before. The report then sets the verdicts against what the
// owner did afterwards: a "handle" item the owner still acted on is one the
// rules would have kept from them by mistake.
//
// It is behind the router_inbox_rules feature flag, which only takes effect
// where the shadow hold runs (router_hold_shadow and the flags it needs), and
// only classifies in workspaces whose settings carry a rule list under
// WorkspaceSettingInboxRules. With either missing, shadow rows are written as
// before, without a verdict.

const (
	// FlagInboxRules is the feature flag (FF_ROUTER_INBOX_RULES). Like
	// FlagInboxResolve it is declared here so the upstream flag file stays
	// untouched.
	FlagInboxRules = "router_inbox_rules"
	// WorkspaceSettingInboxRules is the workspace settings key holding an
	// InboxRulesConfig. Absent or without rules, the workspace is off.
	WorkspaceSettingInboxRules = "router_inbox_rules"

	VerdictHandle = "handle"
	VerdictOwner  = "owner"
	VerdictGray   = "gray"

	// RuleUnmatched names the verdict for an item no rule matches: it goes to
	// the owner, so a gap in the list never counts as handled.
	RuleUnmatched = "unmatched"
	// RuleGrayDefault names the verdict for a gray type no gray rule names.
	RuleGrayDefault = "gray_default"

	// decisionNotePrefix starts a rule verdict in router_hold.decision_note:
	// "rule:<verdict>:<rule name>".
	decisionNotePrefix = "rule:"

	maxInboxRules = 32
	// rulesConfigTTL is how long the hold trusts a workspace's rule list.
	rulesConfigTTL = 30 * time.Second

	DefaultRulesReportWindow = 24 * time.Hour
	DefaultRulesReportSince  = 7 * 24 * time.Hour
	minRulesReportWindow     = time.Hour
	maxRulesReportWindow     = 7 * 24 * time.Hour
)

// InboxGrayTypes are the item types the Router, not a rule, judges (D1 of
// M-117: mentions, and failures the Router may retry). They are always gray:
// a rule may name them only with the gray verdict.
var InboxGrayTypes = []string{"mentioned", "task_failed", "agent_blocked"}

var ruleNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// InboxRule matches an inbox item by type, severity and, for status_changed,
// the status the issue moved to. An empty list matches anything.
type InboxRule struct {
	Name       string   `json:"name"`
	Types      []string `json:"types"`
	Severities []string `json:"severities,omitempty"`
	ToStatuses []string `json:"to_statuses,omitempty"`
	Verdict    string   `json:"verdict"`
}

// InboxRulesConfig is a workspace's rule list. The first matching rule
// decides.
type InboxRulesConfig struct {
	Rules []InboxRule `json:"rules"`
}

// Active reports whether the workspace classifies.
func (c InboxRulesConfig) Active() bool { return len(c.Rules) > 0 }

// Normalize trims and lowercases every list, and rejects a list that could
// misjudge: a "handle" rule must name its types, a gray type may only be
// named by a gray rule, and to_statuses only applies to status_changed.
func (c InboxRulesConfig) Normalize() (InboxRulesConfig, error) {
	if len(c.Rules) > maxInboxRules {
		return InboxRulesConfig{}, fmt.Errorf("at most %d rules", maxInboxRules)
	}
	out := InboxRulesConfig{Rules: make([]InboxRule, 0, len(c.Rules))}
	seen := map[string]bool{}
	for i, r := range c.Rules {
		n := InboxRule{
			Name:       strings.TrimSpace(r.Name),
			Types:      normalizeList(r.Types),
			Severities: normalizeList(r.Severities),
			ToStatuses: normalizeList(r.ToStatuses),
			Verdict:    strings.ToLower(strings.TrimSpace(r.Verdict)),
		}
		if !ruleNamePattern.MatchString(n.Name) || n.Name == RuleUnmatched || n.Name == RuleGrayDefault {
			return InboxRulesConfig{}, fmt.Errorf("rule %d: name must be 1-40 of a-z, 0-9, _ and -, not %q or %q", i+1, RuleUnmatched, RuleGrayDefault)
		}
		if seen[n.Name] {
			return InboxRulesConfig{}, fmt.Errorf("rule %q: duplicate name", n.Name)
		}
		seen[n.Name] = true
		switch n.Verdict {
		case VerdictHandle, VerdictOwner, VerdictGray:
		default:
			return InboxRulesConfig{}, fmt.Errorf("rule %q: verdict must be %s, %s or %s", n.Name, VerdictHandle, VerdictOwner, VerdictGray)
		}
		if n.Verdict == VerdictHandle && len(n.Types) == 0 {
			return InboxRulesConfig{}, fmt.Errorf("rule %q: a %s rule must name its types", n.Name, VerdictHandle)
		}
		if n.Verdict != VerdictGray {
			for _, t := range n.Types {
				if slices.Contains(InboxGrayTypes, t) {
					return InboxRulesConfig{}, fmt.Errorf("rule %q: %s is a gray type; only a %s rule may name it", n.Name, t, VerdictGray)
				}
			}
		}
		if len(n.ToStatuses) > 0 && !slices.Equal(n.Types, []string{"status_changed"}) {
			return InboxRulesConfig{}, fmt.Errorf("rule %q: to_statuses needs types [status_changed]", n.Name)
		}
		out.Rules = append(out.Rules, n)
	}
	return out, nil
}

func normalizeList(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// DefaultInboxRules is the rule list of the M-117 design (Q1), with the
// workspace's statuses that wait for the owner. It is a starting point the
// workspace stores and then edits; classification only ever reads the
// stored list.
func DefaultInboxRules(waitingStatuses []string) InboxRulesConfig {
	return InboxRulesConfig{Rules: []InboxRule{
		{Name: "mention", Types: []string{"mentioned"}, Verdict: VerdictGray},
		{Name: "failure", Types: []string{"task_failed", "agent_blocked"}, Verdict: VerdictGray},
		{Name: "enter_waiting_status", Types: []string{"status_changed"}, ToStatuses: waitingStatuses, Verdict: VerdictOwner},
		{Name: "action_required", Severities: []string{"action_required"}, Verdict: VerdictOwner},
		{Name: "status_progress", Types: []string{"status_changed"}, Verdict: VerdictHandle},
		{Name: "field_update", Types: []string{"priority_changed", "start_date_changed", "due_date_changed"}, Verdict: VerdictHandle},
		{Name: "assignment_churn", Types: []string{"assignee_changed", "unassigned"}, Verdict: VerdictHandle},
		{Name: "subscribed", Types: []string{"issue_subscribed"}, Verdict: VerdictHandle},
		// A comment that @-mentions the owner also writes a "mentioned" item,
		// which the mention rule judges; this is the comment fan-out itself.
		{Name: "comment", Types: []string{"new_comment"}, Verdict: VerdictHandle},
	}}
}

// ParseInboxRulesConfig reads the rule list out of a workspace's settings.
// Anything unreadable or invalid is the zero, inactive config.
func ParseInboxRulesConfig(settings []byte) InboxRulesConfig {
	var values map[string]json.RawMessage
	if json.Unmarshal(settings, &values) != nil {
		return InboxRulesConfig{}
	}
	raw, ok := values[WorkspaceSettingInboxRules]
	if !ok {
		return InboxRulesConfig{}
	}
	var c InboxRulesConfig
	if json.Unmarshal(raw, &c) != nil {
		return InboxRulesConfig{}
	}
	out, err := c.Normalize()
	if err != nil {
		return InboxRulesConfig{}
	}
	return out
}

// LoadInboxRulesConfig reads a workspace's rule list.
func LoadInboxRulesConfig(ctx context.Context, q Querier, workspaceID string) (InboxRulesConfig, error) {
	var settings []byte
	err := q.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, workspaceID).Scan(&settings)
	if errors.Is(err, pgx.ErrNoRows) {
		return InboxRulesConfig{}, ErrNotFound
	}
	if err != nil {
		return InboxRulesConfig{}, err
	}
	return ParseInboxRulesConfig(settings), nil
}

// rulesConfigGeneration moves when this process changes a workspace's rule
// list, so the hold drops its cached copy at once.
var rulesConfigGeneration atomic.Int64

// SetInboxRulesConfig stores a workspace's rule list; nil removes it. The
// caller normalizes it and checks that the statuses exist.
func SetInboxRulesConfig(ctx context.Context, q Querier, workspaceID string, cfg *InboxRulesConfig) error {
	var value any
	if cfg != nil {
		value = cfg
	}
	patch, err := json.Marshal(map[string]any{WorkspaceSettingInboxRules: value})
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
	rulesConfigGeneration.Add(1)
	return nil
}

// InboxVerdict is what the rules decided for one item.
type InboxVerdict struct {
	Verdict string `json:"verdict"`
	Rule    string `json:"rule"`
}

// DecisionNote is the verdict as router_hold.decision_note keeps it.
func (v InboxVerdict) DecisionNote() string {
	return decisionNotePrefix + v.Verdict + ":" + v.Rule
}

// ParseDecisionNote reads a verdict back; ok is false for a note the rules
// did not write.
func ParseDecisionNote(note string) (InboxVerdict, bool) {
	rest, ok := strings.CutPrefix(note, decisionNotePrefix)
	if !ok {
		return InboxVerdict{}, false
	}
	verdict, rule, ok := strings.Cut(rest, ":")
	if !ok || verdict == "" || rule == "" {
		return InboxVerdict{}, false
	}
	return InboxVerdict{Verdict: verdict, Rule: rule}, true
}

// ClassifyInboxItem decides an item's verdict under cfg. A gray type is
// always gray. It fails only when a rule needs the status an item moved to
// and the item's details cannot say.
func ClassifyInboxItem(cfg InboxRulesConfig, p db.CreateInboxItemParams) (InboxVerdict, error) {
	gray := slices.Contains(InboxGrayTypes, p.Type)
	var toStatus *string
	for _, r := range cfg.Rules {
		if gray && r.Verdict != VerdictGray {
			continue
		}
		if len(r.Types) > 0 && !slices.Contains(r.Types, p.Type) {
			continue
		}
		if len(r.Severities) > 0 && !slices.Contains(r.Severities, p.Severity) {
			continue
		}
		if len(r.ToStatuses) > 0 {
			if toStatus == nil {
				s, err := detailsTo(p.Details)
				if err != nil {
					return InboxVerdict{}, err
				}
				toStatus = &s
			}
			if !slices.Contains(r.ToStatuses, *toStatus) {
				continue
			}
		}
		return InboxVerdict{Verdict: r.Verdict, Rule: r.Name}, nil
	}
	if gray {
		return InboxVerdict{Verdict: VerdictGray, Rule: RuleGrayDefault}, nil
	}
	return InboxVerdict{Verdict: VerdictOwner, Rule: RuleUnmatched}, nil
}

// detailsTo is the "to" of a status_changed item's details.
func detailsTo(details []byte) (string, error) {
	var d struct {
		To *string `json:"to"`
	}
	if err := json.Unmarshal(details, &d); err != nil {
		return "", fmt.Errorf("read status_changed details: %w", err)
	}
	if d.To == nil {
		return "", errors.New("status_changed details carry no \"to\"")
	}
	return strings.ToLower(*d.To), nil
}

// inboxRules is the hold's classifier: a per-workspace cache of rule lists
// and the counts of what it decided.
type inboxRules struct {
	mu      sync.Mutex
	configs map[string]rulesConfigEntry
	// rule is ClassifyInboxItem; tests replace it to make it fail.
	rule func(InboxRulesConfig, db.CreateInboxItemParams) (InboxVerdict, error)

	verdicts sync.Map // verdict -> *atomic.Int64
	failures atomic.Int64
}

type rulesConfigEntry struct {
	cfg      InboxRulesConfig
	gen      int64
	loadedAt time.Time
}

func newInboxRules() *inboxRules {
	return &inboxRules{configs: map[string]rulesConfigEntry{}, rule: ClassifyInboxItem}
}

func (r *inboxRules) config(ctx context.Context, q Querier, workspaceID string) (InboxRulesConfig, error) {
	gen := rulesConfigGeneration.Load()
	r.mu.Lock()
	entry, ok := r.configs[workspaceID]
	r.mu.Unlock()
	if ok && entry.gen == gen && time.Since(entry.loadedAt) < rulesConfigTTL {
		return entry.cfg, nil
	}
	cfg, err := LoadInboxRulesConfig(ctx, q, workspaceID)
	if err != nil {
		return InboxRulesConfig{}, err
	}
	r.mu.Lock()
	r.configs[workspaceID] = rulesConfigEntry{cfg: cfg, gen: gen, loadedAt: time.Now()}
	r.mu.Unlock()
	return cfg, nil
}

// classify returns the decision note for p, or nil when the workspace has no
// rules or classifying failed. A failure is counted and returned for the
// log; the caller records the shadow row without a verdict and the inbox
// write is not affected either way.
func (r *inboxRules) classify(ctx context.Context, q Querier, p db.CreateInboxItemParams) (note *string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			note, err = nil, fmt.Errorf("inbox rules: panic: %v", rec)
		}
		if err != nil {
			r.failures.Add(1)
		}
	}()
	cfg, err := r.config(ctx, q, util.UUIDToString(p.WorkspaceID))
	if err != nil {
		return nil, fmt.Errorf("inbox rules: load: %w", err)
	}
	if !cfg.Active() {
		return nil, nil
	}
	v, err := r.rule(cfg, p)
	if err != nil {
		return nil, fmt.Errorf("inbox rules: classify %s: %w", p.Type, err)
	}
	n, _ := r.verdicts.LoadOrStore(v.Verdict, new(atomic.Int64))
	n.(*atomic.Int64).Add(1)
	s := v.DecisionNote()
	return &s, nil
}

// InboxRulesStats are this process's classification counts since it started.
type InboxRulesStats struct {
	Verdicts map[string]int64 `json:"verdicts"`
	Failures int64            `json:"failures"`
}

func (r *inboxRules) stats() *InboxRulesStats {
	if r == nil {
		return nil
	}
	out := &InboxRulesStats{Verdicts: map[string]int64{}, Failures: r.failures.Load()}
	r.verdicts.Range(func(k, v any) bool {
		out.Verdicts[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// RulesReportFilter selects the classified rows a report covers, as
// HoldFilter does for the list.
type RulesReportFilter struct {
	WorkspaceID     string
	RecipientUserID string
	AgentID         string
	Since           time.Duration
	Window          time.Duration
}

// RulesReportDefinition is what "the owner acted" means in the report.
const RulesReportDefinition = "The owner acted on an item when, from the item's arrival until the window ends or " +
	"the next inbox item for the owner on the same issue arrives (whichever is first), the owner commented on " +
	"the issue or changed it (any activity_log row the owner is the actor of: status, assignee, priority, dates, " +
	"and so on). Reading or archiving the item does not count, nor does a reply given elsewhere (in a chat) " +
	"that someone else carries to the issue. An item counts once its window has passed; items still inside it " +
	"are pending. Items that never reached the inbox are left out."

// RuleReport is one rule's row in the report. Rate is Acted / Samples, nil
// without samples; for a "handle" rule it is the mis-hold rate.
type RuleReport struct {
	Rule    string   `json:"rule"`
	Verdict string   `json:"verdict"`
	Samples int64    `json:"samples"`
	Acted   int64    `json:"acted"`
	Rate    *float64 `json:"rate"`
	Pending int64    `json:"pending"`
}

// RulesReport sets the rule verdicts against what the owner did.
type RulesReport struct {
	Since        time.Time    `json:"since"`
	Window       string       `json:"window"`
	Definition   string       `json:"definition"`
	Rules        []RuleReport `json:"rules"`
	Verdicts     []RuleReport `json:"verdicts"`
	Unclassified int64        `json:"unclassified"`
	NotDelivered int64        `json:"not_delivered"`
}

// NormalizeRulesReport fills in the defaults and bounds the filter.
func NormalizeRulesReport(f RulesReportFilter) (RulesReportFilter, error) {
	if f.Window == 0 {
		f.Window = DefaultRulesReportWindow
	}
	if f.Since == 0 {
		f.Since = DefaultRulesReportSince
	}
	if f.Window < minRulesReportWindow || f.Window > maxRulesReportWindow {
		return f, fmt.Errorf("%w: window must be between %s and %s", ErrInvalid, minRulesReportWindow, maxRulesReportWindow)
	}
	if f.Since < f.Window || f.Since > holdRetention {
		return f, fmt.Errorf("%w: since must be between the window and %s", ErrInvalid, holdRetention)
	}
	return f, nil
}

// InboxRulesReport counts, per rule, the classified items of the last
// f.Since and how many of them the owner acted on (RulesReportDefinition).
func (s *Store) InboxRulesReport(ctx context.Context, f RulesReportFilter) (RulesReport, error) {
	f, err := NormalizeRulesReport(f)
	if err != nil {
		return RulesReport{}, err
	}
	since := time.Now().Add(-f.Since)
	out := RulesReport{Since: since, Window: f.Window.String(), Definition: RulesReportDefinition, Rules: []RuleReport{}, Verdicts: []RuleReport{}}
	rows, err := s.DB.Query(ctx, `
		WITH h AS (
		    SELECT h.id, h.recipient_user_id, h.issue_id, h.decision_note,
		           ii.id AS item_id, ii.created_at AS item_at
		    FROM router_hold h
		    LEFT JOIN inbox_item ii ON ii.id = h.inbox_item_id
		    WHERE h.workspace_id = $1::uuid AND h.created_at >= $2
		      AND ($3 = '' OR h.recipient_user_id = NULLIF($3, '')::uuid)
		      AND ($4 = '' OR h.agent_id = NULLIF($4, '')::uuid)
		), judged AS (
		    SELECT h.decision_note,
		           h.item_at + make_interval(secs => $5) <= now() AS mature,
		           EXISTS (
		               SELECT 1 FROM comment c
		               WHERE c.issue_id = h.issue_id AND c.author_type = 'member' AND c.author_id = h.recipient_user_id
		                 AND c.created_at >= h.item_at AND c.created_at < w.until
		           ) OR EXISTS (
		               SELECT 1 FROM activity_log a
		               WHERE a.issue_id = h.issue_id AND a.actor_type = 'member' AND a.actor_id = h.recipient_user_id
		                 AND a.created_at >= h.item_at AND a.created_at < w.until
		           ) AS acted
		    FROM h
		    CROSS JOIN LATERAL (
		        SELECT LEAST(h.item_at + make_interval(secs => $5), (
		            SELECT n.created_at FROM inbox_item n
		            WHERE n.workspace_id = $1::uuid AND n.recipient_type = 'member'
		              AND n.recipient_id = h.recipient_user_id AND n.issue_id = h.issue_id
		              AND (n.created_at, n.id) > (h.item_at, h.item_id)
		            ORDER BY n.created_at, n.id LIMIT 1)) AS until
		    ) w
		    WHERE h.item_at IS NOT NULL AND h.decision_note LIKE 'rule:%'
		)
		SELECT 'rule' AS kind, decision_note,
		       count(*) FILTER (WHERE mature), count(*) FILTER (WHERE mature AND acted), count(*) FILTER (WHERE NOT mature)
		FROM judged GROUP BY decision_note
		UNION ALL
		SELECT 'unclassified', '', count(*) FILTER (WHERE decision_note IS NULL OR decision_note NOT LIKE 'rule:%'), 0, 0 FROM h
		UNION ALL
		SELECT 'not_delivered', '', count(*) FILTER (WHERE item_at IS NULL AND decision_note LIKE 'rule:%'), 0, 0 FROM h`,
		f.WorkspaceID, since, f.RecipientUserID, f.AgentID, f.Window.Seconds())
	if err != nil {
		return RulesReport{}, err
	}
	defer rows.Close()
	byVerdict := map[string]*RuleReport{}
	for rows.Next() {
		var kind, note string
		var samples, acted, pending int64
		if err := rows.Scan(&kind, &note, &samples, &acted, &pending); err != nil {
			return RulesReport{}, err
		}
		switch kind {
		case "unclassified":
			out.Unclassified = samples
			continue
		case "not_delivered":
			out.NotDelivered = samples
			continue
		}
		v, ok := ParseDecisionNote(note)
		if !ok {
			continue
		}
		out.Rules = append(out.Rules, RuleReport{Rule: v.Rule, Verdict: v.Verdict, Samples: samples, Acted: acted, Pending: pending})
		t := byVerdict[v.Verdict]
		if t == nil {
			t = &RuleReport{Verdict: v.Verdict}
			byVerdict[v.Verdict] = t
		}
		t.Samples += samples
		t.Acted += acted
		t.Pending += pending
	}
	if err := rows.Err(); err != nil {
		return RulesReport{}, err
	}
	verdictOrder := []string{VerdictHandle, VerdictGray, VerdictOwner}
	rank := func(v string) int {
		if i := slices.Index(verdictOrder, v); i >= 0 {
			return i
		}
		return len(verdictOrder)
	}
	slices.SortFunc(out.Rules, func(a, b RuleReport) int {
		if d := rank(a.Verdict) - rank(b.Verdict); d != 0 {
			return d
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	for i := range out.Rules {
		out.Rules[i].Rate = actedRate(out.Rules[i].Acted, out.Rules[i].Samples)
	}
	for _, v := range verdictOrder {
		if t := byVerdict[v]; t != nil {
			t.Rate = actedRate(t.Acted, t.Samples)
			out.Verdicts = append(out.Verdicts, *t)
		}
	}
	return out, nil
}

func actedRate(acted, samples int64) *float64 {
	if samples == 0 {
		return nil
	}
	r := float64(acted) / float64(samples)
	return &r
}

// CurrentInboxRulesStats returns the rule counts of the hold this process
// registered, or false when rule classification is off here.
func CurrentInboxRulesStats() (*InboxRulesStats, bool) {
	h := activeHold.Load()
	if h == nil || h.rules == nil {
		return nil, false
	}
	return h.rules.stats(), true
}
