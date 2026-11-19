// Added by the mouse9527/multica fork: Evolution P2, the daily schedule (M-136).

package evolution

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// JobName is the scheduler's audit key for the Evolution tick. Stable across
// releases.
const JobName = "evolution_tick"

// TickInterval is how often the tick looks at every workspace with an
// Evolution agent. A scheduled run starts within one interval of its slot.
const TickInterval = 5 * time.Minute

// Settling a run that never got going.
const (
	// issueGrace is how long a run may sit without its analysis issue (the
	// server stopped between creating the run and the issue).
	issueGrace = 10 * time.Minute
	// taskGrace is how long an analysis issue may sit without any task (the
	// agent's runtime refused the work).
	taskGrace = 30 * time.Minute
)

// JobRegistrar is the scheduler manager's registration method.
type JobRegistrar interface {
	Register(spec scheduler.JobSpec) error
}

// IssueCreator creates the analysis issue; *service.IssueService in the
// server.
type IssueCreator interface {
	Create(ctx context.Context, p service.IssueCreateParams, opts service.IssueCreateOpts) (service.IssueCreateResult, error)
}

// Scheduler is the Evolution tick: it keeps cursors in line with each
// workspace's configuration, tells new members, settles finished runs, and
// starts the next run when one is due.
type Scheduler struct {
	DB      Database
	Queries *db.Queries
	Issues  IssueCreator
	Inbox   inboxwriter.Publisher
	Flags   *featureflag.Service
	Now     func() time.Time
}

// Register adds the Evolution tick to the scheduler when evolution_v1 is on
// at startup. With it off nothing is registered, as upstream. The tick also
// reads the flag on every run, so turning it off stops the schedule without a
// restart. It returns whether it registered the job.
func Register(ctx context.Context, flags *featureflag.Service, database Database, queries *db.Queries, jobs JobRegistrar, issues IssueCreator, bus *events.Bus) bool {
	if !Enabled(ctx, flags) {
		return false
	}
	s := &Scheduler{DB: database, Queries: queries, Issues: issues, Inbox: bus, Flags: flags}
	if err := jobs.Register(s.Job()); err != nil {
		slog.Error("evolution: register tick job", "error", err)
		return false
	}
	slog.Info("evolution: schedule enabled", "interval", TickInterval)
	return true
}

// Job is the tick on the DB-backed scheduler, which leases each plan to one
// replica. A run is created under a per-workspace lock, so an overlapping
// tick cannot open a second one.
func (s *Scheduler) Job() scheduler.JobSpec {
	return scheduler.JobSpec{
		Name:              JobName,
		Cadence:           TickInterval,
		CatchUpMode:       scheduler.CatchUpLatestOnly,
		CatchUpWindow:     time.Hour,
		RunTimeout:        4 * time.Minute,
		StaleTimeout:      5 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       1,
		Scopes:            scheduler.StaticScopes(scheduler.ScopeGlobal),
		Handler: func(ctx context.Context, _ scheduler.HandlerInput) (scheduler.HandlerResult, error) {
			res, err := s.Tick(ctx)
			return scheduler.HandlerResult{
				RowsAffected: int64(res.Started),
				Result: map[string]any{
					"workspaces": res.Workspaces, "started": res.Started, "settled": res.Settled,
					"notified": res.Notified, "errors": res.Errors,
				},
			}, err
		},
	}
}

// TickResult counts what one tick did.
type TickResult struct {
	Workspaces int
	Started    int
	Settled    int
	Notified   int
	Errors     int
}

type workspaceAgent struct {
	WorkspaceID string
	AgentID     string
	AgentName   string
	Settings    []byte
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

// Tick runs once over every workspace that has an Evolution agent. One
// workspace's failure does not stop the others.
func (s *Scheduler) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult
	if !Enabled(ctx, s.Flags) {
		return res, nil
	}
	// A workspace whose agent is gone reads nothing and keeps no open run.
	if _, err := s.DB.Exec(ctx, `
		DELETE FROM evolution_cursor c WHERE NOT EXISTS (
			SELECT 1 FROM agent a WHERE a.workspace_id = c.workspace_id
			  AND a.system_key = 'evolution' AND a.archived_at IS NULL)`); err != nil {
		return res, err
	}
	if _, err := s.DB.Exec(ctx, `
		UPDATE evolution_run r SET status = 'failed', failure_reason = 'the Evolution agent was removed', finished_at = now()
		WHERE r.status = 'open' AND NOT EXISTS (
			SELECT 1 FROM agent a WHERE a.id = r.agent_id AND a.archived_at IS NULL)`); err != nil {
		return res, err
	}
	agents, err := s.listAgents(ctx)
	if err != nil {
		return res, err
	}
	for _, wa := range agents {
		res.Workspaces++
		if err := s.tickWorkspace(ctx, wa, &res); err != nil {
			res.Errors++
			slog.Warn("evolution: tick workspace failed", "workspace_id", wa.WorkspaceID, "error", err)
		}
	}
	return res, nil
}

func (s *Scheduler) listAgents(ctx context.Context) ([]workspaceAgent, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT a.workspace_id::text, a.id::text, a.name, w.settings
		FROM agent a JOIN workspace w ON w.id = a.workspace_id
		WHERE a.system_key = 'evolution' AND a.archived_at IS NULL
		ORDER BY a.workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workspaceAgent{}
	for rows.Next() {
		var wa workspaceAgent
		if err := rows.Scan(&wa.WorkspaceID, &wa.AgentID, &wa.AgentName, &wa.Settings); err != nil {
			return nil, err
		}
		out = append(out, wa)
	}
	return out, rows.Err()
}

func (s *Scheduler) tickWorkspace(ctx context.Context, wa workspaceAgent, res *TickResult) error {
	cfg := ConfigFromSettings(wa.Settings)
	now := s.now()

	// Keep cursors in line with the configuration, whichever way it was
	// written: a source that is off has no cursor, so turning it on again
	// starts from that moment and nothing in between is ever read.
	if !cfg.ChatWanted() {
		if err := DeleteCursor(ctx, s.DB, wa.WorkspaceID, SourceChat, SourceMemberNotice); err != nil {
			return err
		}
	}
	if cfg.CommentsWanted() {
		if err := StartCursorIfMissing(ctx, s.DB, wa.WorkspaceID, SourceComment, Position{TS: now}); err != nil {
			return err
		}
	} else if err := DeleteCursor(ctx, s.DB, wa.WorkspaceID, SourceComment); err != nil {
		return err
	}

	cursors := map[string]Position{}
	for _, source := range []string{SourceComment, SourceChat} {
		p, ok, err := GetCursor(ctx, s.DB, wa.WorkspaceID, source)
		if err != nil {
			return err
		}
		if ok {
			cursors[source] = p
		}
	}

	// Members who joined while chats are read are told too.
	if _, ok := cursors[SourceChat]; ok {
		n, err := s.notifyNewMembers(ctx, wa)
		res.Notified += n
		if err != nil {
			return err
		}
	}

	open, hasOpen, err := OpenRun(ctx, s.DB, wa.WorkspaceID)
	if err != nil {
		return err
	}
	if hasOpen {
		settled, err := s.settle(ctx, open, now)
		if err != nil {
			return err
		}
		if !settled {
			return nil
		}
		res.Settled++
		// Settling may have moved the cursors.
		for source := range cursors {
			p, ok, err := GetCursor(ctx, s.DB, wa.WorkspaceID, source)
			if err != nil {
				return err
			}
			if ok {
				cursors[source] = p
			}
		}
	}

	in := scheduleInput{Now: now, Config: cfg, Cursors: cursors}
	if in.LastSlot, err = lastScheduledSlot(ctx, s.DB, wa.WorkspaceID); err != nil {
		return err
	}
	last, hasLast, err := LatestRun(ctx, s.DB, wa.WorkspaceID)
	if err != nil {
		return err
	}
	if hasLast {
		in.LastRun = &last
	}
	if in.RunsToday, err = countRunsSince(ctx, s.DB, wa.WorkspaceID, localMidnight(now, cfg)); err != nil {
		return err
	}
	plan, due := planRun(in)
	if !due {
		return nil
	}
	started, err := s.startRun(ctx, wa, plan, now, cursors)
	if started {
		res.Started++
	}
	return err
}

func (s *Scheduler) notifyNewMembers(ctx context.Context, wa workspaceAgent) (int, error) {
	from, ok, err := GetCursor(ctx, s.DB, wa.WorkspaceID, SourceMemberNotice)
	if err != nil || !ok {
		return 0, err
	}
	reached, n, err := notifyMembersAfter(ctx, s.DB, s.Queries, s.Inbox, wa.WorkspaceID, Actor{Type: "agent", ID: wa.AgentID}, from)
	if n > 0 {
		if serr := SetCursor(ctx, s.DB, wa.WorkspaceID, SourceMemberNotice, reached); serr != nil && err == nil {
			err = serr
		}
	}
	return n, err
}

// settle closes an open run once its outcome is known and reports whether
// it did. On success each source's cursor moves to what the run was served;
// on failure nothing moves, so the next run reads the same rows again.
func (s *Scheduler) settle(ctx context.Context, run Run, now time.Time) (bool, error) {
	if run.IssueID == "" {
		if now.Sub(run.CreatedAt) < issueGrace {
			return false, nil
		}
		return true, finishRun(ctx, s.DB, run.ID, RunFailed, "the analysis issue was not created")
	}
	rows, err := s.DB.Query(ctx, `
		SELECT status FROM agent_task_queue
		WHERE issue_id = $1::uuid AND agent_id = $2::uuid
		ORDER BY created_at DESC, id DESC`, run.IssueID, run.AgentID)
	if err != nil {
		return false, err
	}
	statuses := []string{}
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			rows.Close()
			return false, err
		}
		statuses = append(statuses, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, st := range statuses {
		switch st {
		case "completed", "failed", "cancelled":
		default:
			// Queued, deferred, dispatched, running, or waiting: not over yet.
			return false, nil
		}
	}
	if len(statuses) == 0 {
		if now.Sub(run.CreatedAt) < taskGrace {
			return false, nil
		}
		return true, finishRun(ctx, s.DB, run.ID, RunFailed, "no task was queued for the analysis issue; check the Evolution agent's runtime")
	}
	if statuses[0] != "completed" {
		return true, finishRun(ctx, s.DB, run.ID, RunFailed, "the analysis task "+statuses[0])
	}
	return true, s.succeed(ctx, run)
}

func (s *Scheduler) succeed(ctx context.Context, run Run) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for source, p := range run.Served {
		if err := advanceCursor(ctx, tx, run.WorkspaceID, source, p); err != nil {
			return err
		}
	}
	if err := finishRun(ctx, tx, run.ID, RunSucceeded, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// startRun opens a run and creates its analysis issue, assigned to the
// Evolution agent, which queues the agent's task.
func (s *Scheduler) startRun(ctx context.Context, wa workspaceAgent, plan runPlan, now time.Time, cursors map[string]Position) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "evolution-run:"+wa.WorkspaceID); err != nil {
		return false, err
	}
	if _, open, err := OpenRun(ctx, tx, wa.WorkspaceID); err != nil || open {
		return false, err
	}
	run, err := insertRun(ctx, tx, wa.WorkspaceID, wa.AgentID, plan.Kind, plan.Slot, now.Add(-SafetyDelay))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	wsID, agentID := util.MustParseUUID(wa.WorkspaceID), util.MustParseUUID(wa.AgentID)
	created, err := s.Issues.Create(ctx, service.IssueCreateParams{
		WorkspaceID:    wsID,
		Title:          runTitle(wa.AgentName, plan, now, ConfigFromSettings(wa.Settings)),
		Description:    pgtype.Text{String: runDescription(run, cursors), Valid: true},
		Status:         "todo",
		Priority:       "none",
		AssigneeType:   pgtype.Text{String: "agent", Valid: true},
		AssigneeID:     agentID,
		CreatorType:    "agent",
		CreatorID:      agentID,
		AllowDuplicate: true,
	}, service.IssueCreateOpts{ActorID: wa.AgentID, AnalyticsAgentID: wa.AgentID})
	if err != nil {
		if ferr := finishRun(ctx, s.DB, run.ID, RunFailed, "could not create the analysis issue"); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return true, fmt.Errorf("create analysis issue: %w", err)
	}
	if err := setRunIssue(ctx, s.DB, run.ID, util.UUIDToString(created.Issue.ID)); err != nil {
		return true, err
	}
	slog.Info("evolution: run started", "workspace_id", wa.WorkspaceID, "run_id", run.ID,
		"kind", plan.Kind, "issue_id", util.UUIDToString(created.Issue.ID))
	return true, nil
}

// Analysis issue titles follow the agent's enable-time language when its
// name is still one of the built-in ones.
var runTitlePrefix = map[string]string{
	"Evolution": "Evolution analysis",
	"进化":        "进化分析",
	"진화":        "진화 분석",
	"進化":        "進化分析",
}

func runTitle(agentName string, plan runPlan, now time.Time, cfg Config) string {
	prefix, ok := runTitlePrefix[agentName]
	if !ok {
		prefix = runTitlePrefix["Evolution"]
	}
	day := now.In(cfg.Location()).Format(time.DateOnly)
	if plan.Slot != nil {
		day = plan.Slot.Format(time.DateOnly)
	}
	if plan.Kind == RunCatchUp {
		return prefix + " · " + day + " · catch-up"
	}
	return prefix + " · " + day
}

// runDescription is the analysis issue's body: what the run covers and how
// to read it. The Evolution system prompt carries the rest.
func runDescription(run Run, cursors map[string]Position) string {
	var b strings.Builder
	b.WriteString("Scheduled Evolution analysis. Report only: post one report comment on this issue; do not open proposal issues yet.\n\n")
	b.WriteString("## Window\n\n")
	for _, source := range []string{SourceComment, SourceChat} {
		p, ok := cursors[source]
		if !ok {
			fmt.Fprintf(&b, "- `%s`: not read in this workspace\n", source)
			continue
		}
		fmt.Fprintf(&b, "- `%s`: after %s, up to %s at most\n", source,
			p.TS.Format(time.RFC3339), run.WindowUntil.Format(time.RFC3339))
	}
	b.WriteString("\n## Read\n\n")
	b.WriteString("```\nmultica evolution signals --source comment --output json\nmultica evolution signals --source chat --output json\n```\n\n")
	b.WriteString("Repeat each with `--after <next_after>` until `has_more` is false. `limit_reached` means this run's item budget is used up; the rest goes to a catch-up run.\n")
	return b.String()
}
