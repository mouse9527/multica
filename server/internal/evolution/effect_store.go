// Added by the mouse9527/multica fork: Evolution P5, effect measurement
// (M-139).

package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// RegressionInboxType is the inbox item each workspace owner gets when an
// applied proposal made its target worse.
const RegressionInboxType = "evolution_regression"

// measureBatch caps how many applies one tick measures per workspace; the
// rest wait for the next tick.
const measureBatch = 20

// Effect is one measured apply, as evolution_effect stores it.
type Effect struct {
	VersionID       string
	WorkspaceID     string
	ProposalIssueID string
	TargetType      string
	TargetID        string
	AppliedAt       time.Time
	WindowDays      int
	SampleBefore    int
	SampleAfter     int
	MetricsBefore   Metrics
	MetricsAfter    Metrics
	Findings        []Finding
	Verdict         string
	Reason          string
	Notified        bool
}

// dueApplies lists the applies whose after window has passed and that have
// not been measured, oldest first.
func dueApplies(ctx context.Context, q Querier, workspaceID string, appliedBefore time.Time) ([]Version, error) {
	return queryVersions(ctx, q, `
		WHERE workspace_id = $1::uuid AND action = 'apply' AND created_at <= $2
		  AND NOT EXISTS (SELECT 1 FROM evolution_effect e WHERE e.version_id = evolution_config_version.id)
		ORDER BY created_at, id
		LIMIT $3`, workspaceID, appliedBefore, measureBatch)
}

// laterVersions lists the target's version rows written after v.
func laterVersions(ctx context.Context, q Querier, v Version) ([]Version, error) {
	return queryVersions(ctx, q, `
		WHERE workspace_id = $1::uuid AND target_type = $2 AND target_id = $3::uuid
		  AND id <> $4::uuid AND created_at >= $5
		ORDER BY created_at, id`, v.WorkspaceID, v.TargetType, v.TargetID, v.ID, v.CreatedAt)
}

// agentTaskCounts reads an agent's runs queued in [from, until).
func agentTaskCounts(ctx context.Context, q Querier, agentID string, from, until time.Time) (AgentTaskCounts, error) {
	var c AgentTaskCounts
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status IN ('completed', 'failed')),
		       count(*) FILTER (WHERE status = 'failed'),
		       count(*) FILTER (WHERE status IN ('completed', 'failed') AND attempt > 1),
		       COALESCE(avg(EXTRACT(EPOCH FROM (completed_at - started_at)))
		                FILTER (WHERE status = 'completed' AND started_at IS NOT NULL AND completed_at IS NOT NULL), 0)::float8,
		       count(*) FILTER (WHERE status = 'completed' AND started_at IS NOT NULL AND completed_at IS NOT NULL)
		FROM agent_task_queue
		WHERE agent_id = $1::uuid AND created_at >= $2 AND created_at < $3`, agentID, from, until).
		Scan(&c.Finished, &c.Failed, &c.Retried, &c.AvgDurationSecs, &c.Timed)
	return c, err
}

// workflowStatusChanges reads the status changes in [from, until) of the
// issues whose project uses the workflow; the Default workflow also covers
// projects without one (or with one that no longer exists), as
// issueworkflow.ForProjectRow resolves them. Issues outside any project are
// not counted: nothing hands off there. Dwell is the time since the issue's
// previous status change, or since it was created.
func workflowStatusChanges(ctx context.Context, q Querier, workspaceID, workflowID string, from, until time.Time) ([]StatusChange, error) {
	rows, err := q.Query(ctx, `
		WITH wf AS (
			SELECT id, is_default FROM issue_workflow WHERE id = $1::uuid AND workspace_id = $2::uuid
		), issues AS (
			SELECT i.id, i.created_at FROM issue i
			JOIN project p ON p.id = i.project_id AND p.workspace_id = i.workspace_id
			JOIN wf ON p.workflow_id = wf.id OR (wf.is_default AND (p.workflow_id IS NULL OR NOT EXISTS (
				SELECT 1 FROM issue_workflow o WHERE o.id = p.workflow_id AND o.workspace_id = p.workspace_id)))
			WHERE i.workspace_id = $2::uuid
		), changes AS (
			SELECT a.issue_id, a.created_at, a.details->>'from' AS from_key, a.details->>'to' AS to_key,
			       COALESCE(lag(a.created_at) OVER (PARTITION BY a.issue_id ORDER BY a.created_at, a.id), issues.created_at) AS entered_at
			FROM activity_log a JOIN issues ON issues.id = a.issue_id
			WHERE a.workspace_id = $2::uuid AND a.action = 'status_changed' AND a.created_at < $4
		)
		SELECT c.issue_id::text, COALESCE(c.from_key, ''), COALESCE(c.to_key, ''),
		       COALESCE(s.category, c.from_key, ''), EXTRACT(EPOCH FROM (c.created_at - c.entered_at))::float8
		FROM changes c
		LEFT JOIN issue_status s ON s.workspace_id = $2::uuid AND s.key = c.from_key
		WHERE c.created_at >= $3`, workflowID, workspaceID, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatusChange{}
	for rows.Next() {
		var c StatusChange
		if err := rows.Scan(&c.IssueID, &c.From, &c.To, &c.FromCategory, &c.DwellSecs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// windowMetrics measures the target over [from, until). snapshot is the
// target's whitelisted fields as they stood in that window.
func windowMetrics(ctx context.Context, q Querier, v Version, snapshot map[string]string, from, until time.Time) (Metrics, int, error) {
	if v.TargetType == TargetWorkflow {
		changes, err := workflowStatusChanges(ctx, q, v.WorkspaceID, v.TargetID, from, until)
		if err != nil {
			return nil, 0, err
		}
		m, n := WorkflowMetrics(changes, ReturnsOf(snapshot["steps"]))
		return m, n, nil
	}
	counts, err := agentTaskCounts(ctx, q, v.TargetID, from, until)
	if err != nil {
		return nil, 0, err
	}
	m, n := AgentMetrics(counts)
	return m, n, nil
}

// measure draws one apply's effect: the window_days before the apply against
// the window_days after it. It reports whether the apply is still in place
// (no rollback has undone it), which is when a regression is worth telling.
func measure(ctx context.Context, q Querier, v Version, cfg Config) (Effect, bool, error) {
	window := time.Duration(cfg.EffectWindowDays) * 24 * time.Hour
	end := v.CreatedAt.Add(window)
	e := Effect{
		VersionID: v.ID, WorkspaceID: v.WorkspaceID, ProposalIssueID: v.ProposalIssueID,
		TargetType: v.TargetType, TargetID: v.TargetID, AppliedAt: v.CreatedAt, WindowDays: cfg.EffectWindowDays,
		MetricsBefore: Metrics{}, MetricsAfter: Metrics{}, Findings: []Finding{},
	}
	later, err := laterVersions(ctx, q, v)
	if err != nil {
		return e, false, err
	}
	active := true
	for _, l := range later {
		if l.Action == ActionRollback && l.RevertedOf == v.ID {
			active = false
		}
	}
	// A change inside the after window muddles it: measure nothing.
	for _, l := range later {
		if !l.CreatedAt.Before(end) {
			continue
		}
		e.Verdict, e.Reason = EffectNotMeasured, NotMeasuredChangedAgain
		if l.Action == ActionRollback && l.RevertedOf == v.ID {
			e.Reason = NotMeasuredRolledBack
		}
		return e, active, nil
	}
	if e.MetricsBefore, e.SampleBefore, err = windowMetrics(ctx, q, v, v.Before, v.CreatedAt.Add(-window), v.CreatedAt); err != nil {
		return e, active, err
	}
	if e.MetricsAfter, e.SampleAfter, err = windowMetrics(ctx, q, v, v.After, v.CreatedAt, end); err != nil {
		return e, active, err
	}
	e.Findings = CompareMetrics(v.TargetType, e.MetricsBefore, e.MetricsAfter, cfg.RegressionThresholdPercent, cfg.MinEffectSample)
	e.Verdict = Verdict(e.SampleBefore, e.SampleAfter, cfg.MinEffectSample, e.Findings)
	return e, active, nil
}

// insertEffect writes the effect row and reports whether this call wrote it
// (an overlapping tick may have).
func insertEffect(ctx context.Context, q Querier, e Effect) (bool, error) {
	before, _ := json.Marshal(e.MetricsBefore)
	after, _ := json.Marshal(e.MetricsAfter)
	findings, _ := json.Marshal(e.Findings)
	tag, err := q.Exec(ctx, `
		INSERT INTO evolution_effect (version_id, workspace_id, proposal_issue_id, target_type, target_id, applied_at,
		                              window_days, sample_before, sample_after, metrics_before, metrics_after,
		                              findings, verdict, reason, notified)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb, $13,
		        NULLIF($14, ''), $15)
		ON CONFLICT (version_id) DO NOTHING`,
		e.VersionID, e.WorkspaceID, e.ProposalIssueID, e.TargetType, e.TargetID, e.AppliedAt,
		e.WindowDays, e.SampleBefore, e.SampleAfter, string(before), string(after), string(findings),
		e.Verdict, e.Reason, e.Notified)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// setProposalEffect records the latest verdict on the proposal issue.
func setProposalEffect(ctx context.Context, q Querier, workspaceID, issueID, verdict string) error {
	meta, _ := json.Marshal(map[string]string{MetaEffect: verdict})
	_, err := q.Exec(ctx, `
		UPDATE issue SET metadata = metadata || $3::jsonb, revision = revision + 1, updated_at = now()
		WHERE id = $1::uuid AND workspace_id = $2::uuid AND metadata ? 'evolution_fingerprint'
		  AND NOT (metadata @> $3::jsonb)`, issueID, workspaceID, string(meta))
	return err
}

func workspaceOwners(ctx context.Context, q Querier, workspaceID string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT user_id::text FROM member WHERE workspace_id = $1::uuid AND role = 'owner' ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// measureWorkspace measures every apply in the workspace whose after window
// has passed. A regression of an apply still in place gets a "suggest
// rolling back" comment by the Evolution agent on the proposal issue and an
// inbox item for each workspace owner. Nothing is rolled back.
func (s *Scheduler) measureWorkspace(ctx context.Context, wa workspaceAgent, cfg Config, now time.Time) (int, error) {
	window := time.Duration(cfg.EffectWindowDays) * 24 * time.Hour
	due, err := dueApplies(ctx, s.DB, wa.WorkspaceID, now.Add(-window))
	if err != nil {
		return 0, err
	}
	measured := 0
	for _, v := range due {
		e, active, err := measure(ctx, s.DB, v, cfg)
		if err != nil {
			return measured, err
		}
		ok, err := s.recordEffect(ctx, wa, v, cfg, e, active && e.Verdict == EffectRegressed)
		if err != nil {
			return measured, err
		}
		if ok {
			measured++
			slog.Info("evolution: apply measured", "workspace_id", wa.WorkspaceID, "version_id", v.ID,
				"verdict", e.Verdict, "sample_before", e.SampleBefore, "sample_after", e.SampleAfter)
		}
	}
	return measured, nil
}

// recordEffect stores the effect, and the regression notice when notify, in
// one transaction; announcements go out after it commits.
func (s *Scheduler) recordEffect(ctx context.Context, wa workspaceAgent, v Version, cfg Config, e Effect, notify bool) (bool, error) {
	e.Notified = notify
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	inserted, err := insertEffect(ctx, tx, e)
	if err != nil || !inserted {
		return false, err
	}
	if err := setProposalEffect(ctx, tx, v.WorkspaceID, v.ProposalIssueID, e.Verdict); err != nil {
		return false, err
	}
	var deferred inboxwriter.Deferred
	var publishComment func()
	if notify {
		snap, _, err := ReadTarget(ctx, tx, v.WorkspaceID, v.TargetType, v.TargetID, false)
		if err != nil && !errors.Is(err, ErrTargetNotFound) {
			return false, err
		}
		notice := EffectNotice{
			AgentName: wa.AgentName, TargetType: v.TargetType, TargetName: snap.Name, Field: v.Field,
			Version: v.Version, WindowDays: e.WindowDays, SampleBefore: e.SampleBefore, SampleAfter: e.SampleAfter,
			ThresholdPercent: cfg.RegressionThresholdPercent, Findings: e.Findings,
		}
		qtx := s.Queries.WithTx(tx)
		created, err := addComment(ctx, qtx, v.WorkspaceID, v.ProposalIssueID, "agent", wa.AgentID, notice.Body())
		if err != nil {
			return false, err
		}
		publishComment = func() { PublishComment(s.Inbox, v.WorkspaceID, created) }
		owners, err := workspaceOwners(ctx, tx, v.WorkspaceID)
		if err != nil {
			return false, err
		}
		details, _ := json.Marshal(map[string]any{"kind": "effect_regression", "version_id": v.ID})
		for _, owner := range owners {
			if _, _, err := inboxwriter.CreateAndAnnounce(ctx, qtx, &deferred, inboxParams(v, wa.AgentID, owner, notice, details), nil); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if publishComment != nil {
		publishComment()
	}
	deferred.Flush(s.Inbox)
	return true, nil
}

func inboxParams(v Version, agentID, ownerID string, notice EffectNotice, details []byte) db.CreateInboxItemParams {
	return db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: util.MustParseUUID(v.WorkspaceID), RecipientType: "member",
		RecipientID: util.MustParseUUID(ownerID), Type: RegressionInboxType, Severity: "attention",
		IssueID: util.MustParseUUID(v.ProposalIssueID), Title: notice.Title(),
		Body:      pgtype.Text{String: notice.Body(), Valid: true},
		ActorType: pgtype.Text{String: "agent", Valid: true}, ActorID: util.MustParseUUID(agentID), Details: details,
	}
}
