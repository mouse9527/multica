// Added by the mouse9527/multica fork: Evolution P6, the monthly condense
// review (M-140).

package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// ReviewCondense is the one review kind: once a month, agents whose
// instructions are long or repeat themselves are put to the Evolution agent,
// which may propose shorter instructions for each.
const ReviewCondense = "condense"

// RunCondense is the Run kind a condense review files proposals under.
const RunCondense = "condense"

// Review statuses. A skipped review found no agent over a threshold and
// created no issue.
const (
	ReviewOpen      = "open"
	ReviewSucceeded = "succeeded"
	ReviewFailed    = "failed"
	ReviewSkipped   = "skipped"
)

// MaxCondenseCandidates caps the agents one review lists, longest first.
const MaxCondenseCandidates = 20

// condenseMinLineRunes: shorter lines (headings, "- yes", blank) are not
// counted as lines that could repeat.
const condenseMinLineRunes = 12

// InstructionsMeasure is what the review measures of an agent's
// instructions: their length, and how many lines repeat an earlier line.
type InstructionsMeasure struct {
	Runes int `json:"runes"`
	// Lines counts the lines long enough to compare.
	Lines int `json:"lines"`
	// RepeatedLines counts the lines equal to an earlier line, ignoring
	// case, spacing, and list or heading markers.
	RepeatedLines   int `json:"repeated_lines"`
	RepeatedPercent int `json:"repeated_percent"`
}

var lineMarker = regexp.MustCompile(`^(?:[-*+>]|#+|\d+[.)])\s+`)

// normalizeLine folds a line for comparison: no list or heading markers, one
// space between words, lower case.
func normalizeLine(line string) string {
	line = strings.TrimSpace(line)
	for {
		stripped := lineMarker.ReplaceAllString(line, "")
		if stripped == line {
			break
		}
		line = strings.TrimSpace(stripped)
	}
	return strings.ToLower(collapseSpace(line))
}

// MeasureInstructions measures text for the condense review.
func MeasureInstructions(text string) InstructionsMeasure {
	m := InstructionsMeasure{Runes: utf8.RuneCountInString(text)}
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		norm := normalizeLine(line)
		if utf8.RuneCountInString(norm) < condenseMinLineRunes || strings.HasPrefix(norm, "```") {
			continue
		}
		m.Lines++
		if seen[norm] {
			m.RepeatedLines++
		}
		seen[norm] = true
	}
	if m.Lines > 0 {
		m.RepeatedPercent = m.RepeatedLines * 100 / m.Lines
	}
	return m
}

// Exceeds reports whether the measurement puts an agent on the review: long
// instructions, or at least two repeated lines that make up the configured
// share of the lines.
func (m InstructionsMeasure) Exceeds(cfg Config) bool {
	return m.Runes >= cfg.CondenseMinRunes ||
		(m.RepeatedLines >= 2 && m.RepeatedPercent >= cfg.CondenseRepeatPercent)
}

// CondenseCandidate is one agent a review lists.
type CondenseCandidate struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	InstructionsMeasure
}

// Review is one monthly review.
type Review struct {
	ID            string
	WorkspaceID   string
	AgentID       string
	IssueID       string
	Kind          string
	Period        time.Time
	Status        string
	Candidates    []CondenseCandidate
	FailureReason string
	CreatedAt     time.Time
}

// Candidate returns the listed agent with id agentID.
func (r Review) Candidate(agentID string) (CondenseCandidate, bool) {
	for _, c := range r.Candidates {
		if strings.EqualFold(c.AgentID, agentID) {
			return c, true
		}
	}
	return CondenseCandidate{}, false
}

// run is the review as the Run its proposals are filed under: the review's
// id counts its proposals, and it was served no signals.
func (r Review) run() Run {
	return Run{
		ID: r.ID, WorkspaceID: r.WorkspaceID, AgentID: r.AgentID, IssueID: r.IssueID,
		Kind: RunCondense, Status: RunOpen, Served: map[string]Position{}, CreatedAt: r.CreatedAt,
	}
}

const reviewColumns = `id::text, workspace_id::text, agent_id::text, COALESCE(issue_id::text, ''), kind,
	period::timestamp, status, candidates, COALESCE(failure_reason, ''), created_at`

func scanReview(row pgx.Row) (Review, error) {
	var r Review
	var candidates []byte
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.AgentID, &r.IssueID, &r.Kind,
		&r.Period, &r.Status, &candidates, &r.FailureReason, &r.CreatedAt)
	if err != nil {
		return Review{}, err
	}
	r.Candidates = []CondenseCandidate{}
	if len(candidates) > 0 {
		if err := json.Unmarshal(candidates, &r.Candidates); err != nil {
			return Review{}, fmt.Errorf("review %s candidates: %w", r.ID, err)
		}
	}
	return r, nil
}

// openReview returns the workspace's open review, if any.
func openReview(ctx context.Context, q Querier, workspaceID string) (Review, bool, error) {
	r, err := scanReview(q.QueryRow(ctx, `SELECT `+reviewColumns+` FROM evolution_review
		WHERE workspace_id = $1::uuid AND status = 'open' ORDER BY created_at DESC LIMIT 1`, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, false, nil
	}
	return r, err == nil, err
}

// reviewForIssue returns the open review whose issue is issueID.
func reviewForIssue(ctx context.Context, q Querier, workspaceID, issueID string) (Review, error) {
	r, err := scanReview(q.QueryRow(ctx, `SELECT `+reviewColumns+` FROM evolution_review
		WHERE workspace_id = $1::uuid AND issue_id = $2::uuid AND status = 'open'`, workspaceID, issueID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, ErrNoOpenRun
	}
	return r, err
}

func reviewExists(ctx context.Context, q Querier, workspaceID, kind string, period time.Time) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM evolution_review
		               WHERE workspace_id = $1::uuid AND kind = $2 AND period = $3::date)`,
		workspaceID, kind, period.Format(time.DateOnly)).Scan(&exists)
	return exists, err
}

func insertReview(ctx context.Context, q Querier, workspaceID, agentID string, period time.Time, status string, candidates []CondenseCandidate) (Review, error) {
	raw, err := json.Marshal(candidates)
	if err != nil {
		return Review{}, err
	}
	return scanReview(q.QueryRow(ctx, `
		INSERT INTO evolution_review (workspace_id, agent_id, kind, period, status, candidates, finished_at)
		VALUES ($1::uuid, $2::uuid, $3, $4::date, $5::text, $6::jsonb,
		        CASE WHEN $5::text = 'open' THEN NULL ELSE now() END)
		RETURNING `+reviewColumns,
		workspaceID, agentID, ReviewCondense, period.Format(time.DateOnly), status, string(raw)))
}

func setReviewIssue(ctx context.Context, q Querier, reviewID, issueID string) error {
	_, err := q.Exec(ctx, `UPDATE evolution_review SET issue_id = $2::uuid WHERE id = $1::uuid`, reviewID, issueID)
	return err
}

func finishReview(ctx context.Context, q Querier, reviewID, status, reason string) error {
	_, err := q.Exec(ctx, `
		UPDATE evolution_review SET status = $2, failure_reason = NULLIF($3, ''), finished_at = now()
		WHERE id = $1::uuid AND status = 'open'`, reviewID, status, reason)
	return err
}

// workspaceVisibleAgent is the SQL condition (over agent a) for an agent
// every member can see: public_to with the whole workspace as a target, the
// same boundary the signal channel applies to chats. A review issue and its
// proposals are read by every member, so a private agent, or one shared with
// only some members, never appears in them.
const workspaceVisibleAgent = `a.permission_mode = 'public_to'
		  AND EXISTS (SELECT 1 FROM agent_invocation_target t
		              WHERE t.agent_id = a.id AND t.target_type = 'workspace')`

// condenseCandidates measures every agent a proposal may target (not
// Evolution, not internal system agents, not archived, not excluded) that
// every member can see, and returns those over a threshold, longest first.
func condenseCandidates(ctx context.Context, q Querier, workspaceID string, cfg Config) ([]CondenseCandidate, error) {
	rows, err := q.Query(ctx, `
		SELECT a.id::text, a.name, a.instructions FROM agent a
		WHERE a.workspace_id = $1::uuid AND a.archived_at IS NULL AND a.kind <> 'system'
		  AND COALESCE(a.system_key, '') <> $2
		  AND NOT (a.id = ANY(COALESCE($3::uuid[], '{}')))
		  AND `+workspaceVisibleAgent+`
		ORDER BY a.id`, workspaceID, service.EvolutionSystemKey, cfg.ExcludeAgents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CondenseCandidate{}
	for rows.Next() {
		var c CondenseCandidate
		var instructions string
		if err := rows.Scan(&c.AgentID, &c.Name, &instructions); err != nil {
			return nil, err
		}
		c.InstructionsMeasure = MeasureInstructions(instructions)
		if c.Exceeds(cfg) {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortCandidates(out)
	if len(out) > MaxCondenseCandidates {
		out = out[:MaxCondenseCandidates]
	}
	return out, nil
}

// agentVisibleToWorkspace reports whether every member of the workspace can
// see the agent (workspaceVisibleAgent).
func agentVisibleToWorkspace(ctx context.Context, q Querier, workspaceID, agentID string) (bool, error) {
	var visible bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM agent a
		               WHERE a.id = $1::uuid AND a.workspace_id = $2::uuid AND `+workspaceVisibleAgent+`)`,
		agentID, workspaceID).Scan(&visible)
	return visible, err
}

// sortCandidates puts the longest instructions first, ties by id so the
// list is stable.
func sortCandidates(c []CondenseCandidate) {
	sort.Slice(c, func(i, j int) bool {
		if c[i].Runes != c[j].Runes {
			return c[i].Runes > c[j].Runes
		}
		return c[i].AgentID < c[j].AgentID
	})
}

// condensePeriod returns the month (its first day, midnight UTC) whose
// review is current at now, and whether that month's review time (the
// configured day at the schedule's time) has come. A month whose time came
// while the server was down is reviewed as soon as it is back; earlier
// months are not caught up.
func condensePeriod(now time.Time, cfg Config) (time.Time, bool) {
	loc := cfg.Location()
	local := now.In(loc)
	hour, minute := parseClock(cfg.Schedule.Time)
	at := time.Date(local.Year(), local.Month(), cfg.CondenseDayOfMonth, hour, minute, 0, 0, loc)
	period := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, time.UTC)
	return period, !local.Before(at)
}

// reviewWorkspace settles the workspace's open review once its task ended,
// and opens this month's review when it is due. It reports how many reviews
// it settled and started.
func (s *Scheduler) reviewWorkspace(ctx context.Context, wa workspaceAgent, cfg Config, now time.Time) (settled, started int, err error) {
	open, hasOpen, err := openReview(ctx, s.DB, wa.WorkspaceID)
	if err != nil {
		return 0, 0, err
	}
	if hasOpen {
		done, err := s.settleReview(ctx, open, now)
		if err != nil || !done {
			return 0, 0, err
		}
		settled = 1
	}
	if !cfg.CondenseWanted() {
		return settled, 0, nil
	}
	period, due := condensePeriod(now, cfg)
	if !due {
		return settled, 0, nil
	}
	ok, err := s.startReview(ctx, wa, cfg, period, now)
	if ok {
		started = 1
	}
	return settled, started, err
}

// settleReview closes an open review once its task ended. Nothing moves:
// the proposals it filed stand either way, and a failed review is not
// retried before next month.
func (s *Scheduler) settleReview(ctx context.Context, r Review, now time.Time) (bool, error) {
	if r.IssueID == "" {
		if now.Sub(r.CreatedAt) < issueGrace {
			return false, nil
		}
		return true, finishReview(ctx, s.DB, r.ID, ReviewFailed, "the review issue was not created")
	}
	done, last, err := s.issueOutcome(ctx, r.IssueID, r.AgentID, r.CreatedAt, now)
	if err != nil || !done {
		return false, err
	}
	switch last {
	case "":
		return true, finishReview(ctx, s.DB, r.ID, ReviewFailed, "no task was queued for the review issue; check the Evolution agent's runtime")
	case "completed":
		return true, finishReview(ctx, s.DB, r.ID, ReviewSucceeded, "")
	default:
		return true, finishReview(ctx, s.DB, r.ID, ReviewFailed, "the review task "+last)
	}
}

// startReview opens the month's review unless it already has one: it
// measures the agents and, when some are over a threshold, creates the
// review issue assigned to the Evolution agent. A month with none is
// recorded as skipped, so it is measured once.
func (s *Scheduler) startReview(ctx context.Context, wa workspaceAgent, cfg Config, period, now time.Time) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "evolution-review:"+wa.WorkspaceID); err != nil {
		return false, err
	}
	if exists, err := reviewExists(ctx, tx, wa.WorkspaceID, ReviewCondense, period); err != nil || exists {
		return false, err
	}
	candidates, err := condenseCandidates(ctx, tx, wa.WorkspaceID, cfg)
	if err != nil {
		return false, err
	}
	status := ReviewOpen
	if len(candidates) == 0 {
		status = ReviewSkipped
	}
	review, err := insertReview(ctx, tx, wa.WorkspaceID, wa.AgentID, period, status, candidates)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if status == ReviewSkipped {
		return false, nil
	}

	agentID := util.MustParseUUID(wa.AgentID)
	created, err := s.Issues.Create(ctx, service.IssueCreateParams{
		WorkspaceID:    util.MustParseUUID(wa.WorkspaceID),
		Title:          reviewTitle(wa.AgentName, period),
		Description:    pgtype.Text{String: reviewDescription(review, cfg), Valid: true},
		Status:         "todo",
		Priority:       "none",
		AssigneeType:   pgtype.Text{String: "agent", Valid: true},
		AssigneeID:     agentID,
		CreatorType:    "agent",
		CreatorID:      agentID,
		AllowDuplicate: true,
	}, service.IssueCreateOpts{ActorID: wa.AgentID, AnalyticsAgentID: wa.AgentID, ServerWrite: true})
	if err != nil {
		if ferr := finishReview(ctx, s.DB, review.ID, ReviewFailed, "could not create the review issue"); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return true, fmt.Errorf("create review issue: %w", err)
	}
	if err := setReviewIssue(ctx, s.DB, review.ID, util.UUIDToString(created.Issue.ID)); err != nil {
		return true, err
	}
	slog.Info("evolution: condense review started", "workspace_id", wa.WorkspaceID, "review_id", review.ID,
		"agents", len(candidates), "issue_id", util.UUIDToString(created.Issue.ID))
	return true, nil
}

// Review issue titles follow the agent's enable-time language like the
// analysis issues'.
var reviewTitlePrefix = map[string]string{
	"Evolution": "Evolution condense review",
	"进化":        "进化精简",
	"진화":        "진화 간결화 검토",
	"進化":        "進化精簡",
}

func reviewTitle(agentName string, period time.Time) string {
	prefix, ok := reviewTitlePrefix[agentName]
	if !ok {
		prefix = reviewTitlePrefix["Evolution"]
	}
	return prefix + " · " + period.Format("2006-01")
}

// reviewDescription is the review issue's body: the agents to condense and
// how. The Evolution system prompt carries the rest.
func reviewDescription(r Review, cfg Config) string {
	var b strings.Builder
	b.WriteString("Monthly Evolution condense review. The instructions of the agents below are long or repeat themselves. ")
	b.WriteString("For each, propose shorter instructions that keep every rule, then post one report comment on this issue. Nothing is applied.\n\n")
	b.WriteString("## Agents\n\n")
	b.WriteString("| Agent | id | Characters | Repeated lines |\n|---|---|---|---|\n")
	for _, c := range r.Candidates {
		fmt.Fprintf(&b, "| %s | `%s` | %d | %d of %d (%d%%) |\n",
			strings.ReplaceAll(c.Name, "|", "/"), c.AgentID, c.Runes, c.RepeatedLines, c.Lines, c.RepeatedPercent)
	}
	fmt.Fprintf(&b, "\nAn agent is listed when its instructions have at least %d characters, or when at least %d%% of their lines (and at least two) repeat an earlier line.\n\n",
		cfg.CondenseMinRunes, cfg.CondenseRepeatPercent)
	b.WriteString("## Read\n\n```\nmultica agent get <id> --output json\n```\n\n")
	b.WriteString("## Propose\n\n```\nmultica evolution propose --file proposal.json\n```\n\n")
	b.WriteString("- `target` is the listed agent, `field` is `instructions`, and `evidence` is empty: the server attaches the measurement above and sets the topic.\n")
	b.WriteString("- `change.before` is the passage you rewrite, copied exactly (the whole instructions when they fit); `change.after` is the shorter text. A change that does not make the instructions shorter is refused.\n")
	b.WriteString("- Merge repeated and overlapping lines, cut filler, keep every rule and its meaning. When nothing can go without changing what the agent does, file nothing for it and say so in the report.\n")
	b.WriteString("\nThe proposal rules are in the `multica-evolution` skill. A refusal says why; fix it or drop the proposal.\n")
	return b.String()
}
