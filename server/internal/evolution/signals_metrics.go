// Added by the mouse9527/multica fork: Evolution structural analysis, the
// read-only metrics source, read by the weekly structural review (M-296).

package evolution

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// SourceMetrics is the aggregate source: numbers the server computes over
// status changes, agent runs, and handoffs. It carries no comment or chat
// text and nothing about members. It has no cursor: every read summarises a
// fixed window ending at the run's window_until.
const SourceMetrics = "metrics"

// Bounds of the metrics window.
const (
	MinMetricsWindowDays     = 1
	MaxMetricsWindowDays     = 30
	DefaultMetricsWindowDays = 14
)

// MetricsShortRunSeconds: a completed run shorter than this counts as short.
const MetricsShortRunSeconds = 45

// MetricsReturnLoopMin is how many returns in the window put an issue on the
// return-loop list; MaxReturnLoops caps the list.
const (
	MetricsReturnLoopMin = 3
	MaxReturnLoops       = 50
)

// Run trigger kinds in AgentSummary.Triggers. Anything else is the task's
// trigger_evidence_kind as stored, or "other".
const (
	TriggerChat      = "chat"
	TriggerAutopilot = "autopilot"
	TriggerComment   = "comment"
	TriggerOther     = "other"
)

// MetricsSummary is one read of the metrics source.
type MetricsSummary struct {
	Source      string            `json:"source"`
	WindowFrom  time.Time         `json:"window_from"`
	WindowUntil time.Time         `json:"window_until"`
	WindowDays  int               `json:"window_days"`
	Workflows   []WorkflowSummary `json:"workflows"`
	Agents      []AgentSummary    `json:"agents"`
	ReturnLoops []ReturnLoop      `json:"return_loops"`
}

// WorkflowSummary is the status flow of the issues whose project uses one
// workflow. ID is empty for the built-in Default of a workspace that never
// stored its own.
type WorkflowSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Issues is how many issues changed status in the window.
	Issues int           `json:"issues"`
	Steps  []StepSummary `json:"steps"`
}

// StepSummary is one status within a workflow over the window.
type StepSummary struct {
	StatusKey  string `json:"status_key"`
	StatusName string `json:"status_name"`
	Category   string `json:"category"`
	// InWorkflow: the status is one of the workflow's steps (an issue can
	// still pass through others by hand).
	InWorkflow bool `json:"in_workflow"`
	// Entries and Exits count status changes into and out of the status.
	Entries int            `json:"entries"`
	Exits   int            `json:"exits"`
	ExitsTo map[string]int `json:"exits_to"`
	// Dwell is how long an issue stayed before each exit. Not measured for
	// backlog, done, and cancelled, where issues rest.
	DwellSample   int      `json:"dwell_sample"`
	DwellP50Hours *float64 `json:"dwell_p50_hours"`
	DwellP90Hours *float64 `json:"dwell_p90_hours"`
	// BackStatusKey is where the step's "needs changes" sends an issue; a
	// return is an exit to exactly that status. Empty when the step has no
	// way back, and then returns are not measured.
	BackStatusKey string   `json:"back_status_key,omitempty"`
	Returns       int      `json:"returns"`
	ReturnRate    *float64 `json:"return_rate"`
}

// AgentSummary is one agent's runs queued in the window, and the issues
// handed to it or away from it.
type AgentSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
	Runs     int    `json:"runs"`
	// Finished counts completed, failed, and cancelled runs; the rates are
	// over it.
	Finished    int      `json:"finished"`
	Completed   int      `json:"completed"`
	Failed      int      `json:"failed"`
	Cancelled   int      `json:"cancelled"`
	FailureRate *float64 `json:"failure_rate"`
	CancelRate  *float64 `json:"cancel_rate"`
	// Durations are over completed runs with both timestamps.
	DurationSample  int      `json:"duration_sample"`
	DurationP50Secs *float64 `json:"duration_p50_seconds"`
	ShortRuns       int      `json:"short_runs"`
	ShortRunRate    *float64 `json:"short_run_rate"`
	// Triggers counts runs by what queued them.
	Triggers    map[string]int `json:"triggers"`
	HandoffsIn  int            `json:"handoffs_in"`
	HandoffsOut int            `json:"handoffs_out"`
}

// ReturnLoop is an issue sent back at least MetricsReturnLoopMin times in
// the window.
type ReturnLoop struct {
	IssueID    string         `json:"issue_id"`
	Identifier string         `json:"identifier"`
	WorkflowID string         `json:"workflow_id"`
	Returns    int            `json:"returns"`
	ByStep     map[string]int `json:"by_step"`
}

// MetricsRequest asks for the metrics of the open run on IssueID.
type MetricsRequest struct {
	WorkspaceID string
	IssueID     string
	Days        int
	Config      Config
	// HiddenProjects is projectaccess.Hidden.Param() for the run.
	HiddenProjects []pgtype.UUID
	// SelfAgentID is the Evolution agent; its own runs are not summarised.
	SelfAgentID string
}

// ServeMetrics summarises the Days before the open run's window_until. Only
// the week's structural review run reads it (structure_review.go).
// Issues in hidden (private) or excluded projects, and excluded agents, are
// left out in SQL; so are runs on those issues and chats filed under those
// projects.
func ServeMetrics(ctx context.Context, q Querier, req MetricsRequest) (MetricsSummary, error) {
	if !req.Config.MetricsWanted() {
		return MetricsSummary{}, ErrSourceOff
	}
	run, err := openRunForIssue(ctx, q, req.WorkspaceID, req.IssueID)
	if err != nil {
		return MetricsSummary{}, err
	}
	if structure, err := isStructureRun(ctx, q, run); err != nil {
		return MetricsSummary{}, err
	} else if !structure {
		return MetricsSummary{}, ErrNotStructureRun
	}
	days := req.Days
	if days == 0 {
		days = DefaultMetricsWindowDays
	}
	days = clampInt(days, MinMetricsWindowDays, MaxMetricsWindowDays)
	until := run.WindowUntil
	from := until.Add(-time.Duration(days) * 24 * time.Hour)
	out := MetricsSummary{Source: SourceMetrics, WindowFrom: from, WindowUntil: until, WindowDays: days}

	workflows, err := metricsWorkflowsOf(ctx, q, req.WorkspaceID)
	if err != nil {
		return out, err
	}
	statuses, err := metricsStatusesOf(ctx, q, req.WorkspaceID)
	if err != nil {
		return out, err
	}
	changes, err := metricsStatusChanges(ctx, q, req.WorkspaceID, from, until, req.HiddenProjects, req.Config.ExcludeProjects)
	if err != nil {
		return out, err
	}
	out.Workflows, out.ReturnLoops = SummarizeWorkflows(changes, workflows, statuses)
	if err := fillLoopIdentifiers(ctx, q, out.ReturnLoops); err != nil {
		return out, err
	}

	excluded := append([]string{req.SelfAgentID}, req.Config.ExcludeAgents...)
	agents, err := metricsAgentsOf(ctx, q, req.WorkspaceID, excluded)
	if err != nil {
		return out, err
	}
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	tasks, err := metricsTasks(ctx, q, ids, from, until, req.HiddenProjects, req.Config.ExcludeProjects)
	if err != nil {
		return out, err
	}
	handoffs, err := ReadHandoffs(ctx, q, req.WorkspaceID, from, until, req.HiddenProjects, req.Config.ExcludeProjects)
	if err != nil {
		return out, err
	}
	out.Agents = SummarizeAgents(agents, tasks, handoffs)
	return out, nil
}

// MetricsWorkflow is a workflow as the summary needs it.
type MetricsWorkflow struct {
	ID    string
	Name  string
	Steps []issueworkflow.Step
}

// MetricsStatus is a workspace status.
type MetricsStatus struct {
	Name     string
	Category string
}

// MetricsChange is one status change in the window, with the workflow its
// issue's project uses ("" for the built-in Default). An empty FromCategory
// is looked up in the workspace's statuses.
type MetricsChange struct {
	StatusChange
	WorkflowID string
}

// MetricsAgent is an agent the summary covers.
type MetricsAgent struct {
	ID       string
	Name     string
	Archived bool
}

// MetricsTask is one run queued in the window. DurationSecs is set only for
// a completed run with both timestamps.
type MetricsTask struct {
	AgentID      string
	Status       string
	Trigger      string
	DurationSecs *float64
}

// MetricsHandoff is one handoff of an issue in the window (see ReadHandoffs).
type MetricsHandoff struct {
	IssueID          string
	FromType, FromID string
	ToType, ToID     string
}

// SummarizeWorkflows groups the status changes by workflow and status, and
// lists the issues returned at least MetricsReturnLoopMin times (identifiers
// are filled by the caller). Workflows without a change in the window are
// left out.
func SummarizeWorkflows(changes []MetricsChange, workflows []MetricsWorkflow, statuses map[string]MetricsStatus) ([]WorkflowSummary, []ReturnLoop) {
	byID := map[string]MetricsWorkflow{}
	for _, wf := range workflows {
		byID[wf.ID] = wf
	}
	type stepAcc struct {
		s     StepSummary
		dwell []float64
	}
	type wfAcc struct {
		wf      MetricsWorkflow
		back    map[string]string
		issues  map[string]bool
		steps   map[string]*stepAcc
		returns map[string]map[string]int // issue → step → returns
	}
	accs := map[string]*wfAcc{}
	order := []string{}
	step := func(a *wfAcc, key string) *stepAcc {
		if s, ok := a.steps[key]; ok {
			return s
		}
		st := statuses[key]
		category := st.Category
		if category == "" {
			category = key
		}
		s := &stepAcc{s: StepSummary{StatusKey: key, StatusName: st.Name, Category: category, ExitsTo: map[string]int{}}}
		for _, ws := range a.wf.Steps {
			if ws.StatusKey == key {
				s.s.InWorkflow = true
			}
		}
		s.s.BackStatusKey = a.back[key]
		a.steps[key] = s
		return s
	}
	for _, c := range changes {
		a, ok := accs[c.WorkflowID]
		if !ok {
			wf, known := byID[c.WorkflowID]
			if !known {
				wf = MetricsWorkflow{ID: c.WorkflowID}
				if c.WorkflowID == "" {
					wf.Name = issueworkflow.DefaultName
				}
			}
			back := map[string]string{}
			for _, s := range wf.Steps {
				if s.BackStatusKey != "" && s.BackStatusKey != s.StatusKey {
					back[s.StatusKey] = s.BackStatusKey
				}
			}
			a = &wfAcc{wf: wf, back: back, issues: map[string]bool{}, steps: map[string]*stepAcc{}, returns: map[string]map[string]int{}}
			accs[c.WorkflowID] = a
			order = append(order, c.WorkflowID)
		}
		a.issues[c.IssueID] = true
		if c.To != "" {
			step(a, c.To).s.Entries++
		}
		if c.From == "" {
			continue
		}
		from := step(a, c.From)
		from.s.Exits++
		from.s.ExitsTo[c.To]++
		if back, ok := a.back[c.From]; ok && back == c.To {
			from.s.Returns++
			if a.returns[c.IssueID] == nil {
				a.returns[c.IssueID] = map[string]int{}
			}
			a.returns[c.IssueID][c.From]++
		}
		category := c.FromCategory
		if category == "" {
			category = from.s.Category
		}
		if !dwellExcluded[category] && c.DwellSecs >= 0 {
			from.dwell = append(from.dwell, c.DwellSecs/3600)
		}
	}

	sort.Strings(order)
	summaries := make([]WorkflowSummary, 0, len(order))
	loops := []ReturnLoop{}
	for _, id := range order {
		a := accs[id]
		ws := WorkflowSummary{ID: a.wf.ID, Name: a.wf.Name, Issues: len(a.issues), Steps: []StepSummary{}}
		keys := make([]string, 0, len(a.steps))
		for k := range a.steps {
			keys = append(keys, k)
		}
		position := map[string]int{}
		for i, s := range a.wf.Steps {
			position[s.StatusKey] = i + 1
		}
		sort.Slice(keys, func(i, j int) bool {
			pi, pj := position[keys[i]], position[keys[j]]
			if (pi == 0) != (pj == 0) {
				return pi != 0
			}
			if pi != pj {
				return pi < pj
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			s := a.steps[k]
			s.s.DwellSample = len(s.dwell)
			if len(s.dwell) > 0 {
				sorted := append([]float64(nil), s.dwell...)
				sort.Float64s(sorted)
				s.s.DwellP50Hours = ptr(round4(percentile(sorted, 0.5)))
				s.s.DwellP90Hours = ptr(round4(percentile(sorted, 0.9)))
			}
			if s.s.BackStatusKey != "" && s.s.Exits > 0 {
				s.s.ReturnRate = ptr(round4(float64(s.s.Returns) / float64(s.s.Exits)))
			}
			ws.Steps = append(ws.Steps, s.s)
		}
		summaries = append(summaries, ws)
		for issueID, bySteps := range a.returns {
			total := 0
			for _, n := range bySteps {
				total += n
			}
			if total >= MetricsReturnLoopMin {
				loops = append(loops, ReturnLoop{IssueID: issueID, WorkflowID: id, Returns: total, ByStep: bySteps})
			}
		}
	}
	sort.Slice(loops, func(i, j int) bool {
		if loops[i].Returns != loops[j].Returns {
			return loops[i].Returns > loops[j].Returns
		}
		return loops[i].IssueID < loops[j].IssueID
	})
	if len(loops) > MaxReturnLoops {
		loops = loops[:MaxReturnLoops]
	}
	return summaries, loops
}

// SummarizeAgents turns each agent's runs and handoffs into its summary.
// Every listed agent is in the result, in the order given, even with no run.
func SummarizeAgents(agents []MetricsAgent, tasks []MetricsTask, handoffs []MetricsHandoff) []AgentSummary {
	type acc struct {
		s         AgentSummary
		durations []float64
	}
	byID := map[string]*acc{}
	out := make([]*acc, len(agents))
	for i, a := range agents {
		out[i] = &acc{s: AgentSummary{ID: a.ID, Name: a.Name, Archived: a.Archived, Triggers: map[string]int{}}}
		byID[a.ID] = out[i]
	}
	for _, t := range tasks {
		a, ok := byID[t.AgentID]
		if !ok {
			continue
		}
		a.s.Runs++
		trigger := t.Trigger
		if trigger == "" {
			trigger = TriggerOther
		}
		a.s.Triggers[trigger]++
		switch t.Status {
		case "completed":
			a.s.Completed++
		case "failed":
			a.s.Failed++
		case "cancelled":
			a.s.Cancelled++
		}
		if t.Status == "completed" && t.DurationSecs != nil && *t.DurationSecs >= 0 {
			a.durations = append(a.durations, *t.DurationSecs)
			if *t.DurationSecs < MetricsShortRunSeconds {
				a.s.ShortRuns++
			}
		}
	}
	for _, h := range handoffs {
		if h.FromType == "agent" && h.ToType == "agent" && h.FromID == h.ToID {
			continue
		}
		if h.FromType == "agent" {
			if a, ok := byID[h.FromID]; ok {
				a.s.HandoffsOut++
			}
		}
		if h.ToType == "agent" {
			if a, ok := byID[h.ToID]; ok {
				a.s.HandoffsIn++
			}
		}
	}
	result := make([]AgentSummary, len(out))
	for i, a := range out {
		a.s.Finished = a.s.Completed + a.s.Failed + a.s.Cancelled
		if a.s.Finished > 0 {
			a.s.FailureRate = ptr(round4(float64(a.s.Failed) / float64(a.s.Finished)))
			a.s.CancelRate = ptr(round4(float64(a.s.Cancelled) / float64(a.s.Finished)))
		}
		a.s.DurationSample = len(a.durations)
		if len(a.durations) > 0 {
			sorted := append([]float64(nil), a.durations...)
			sort.Float64s(sorted)
			a.s.DurationP50Secs = ptr(round4(percentile(sorted, 0.5)))
			a.s.ShortRunRate = ptr(round4(float64(a.s.ShortRuns) / float64(len(a.durations))))
		}
		result[i] = a.s
	}
	return result
}

// percentile interpolates linearly between the closest ranks, as Postgres'
// percentile_cont does. sorted must be ascending and non-empty.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

func ptr(v float64) *float64 { return &v }

// openRunForIssue is the open run whose analysis issue is issueID. Reading
// metrics moves nothing, so the row is not locked.
func openRunForIssue(ctx context.Context, q Querier, workspaceID, issueID string) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM evolution_run
		WHERE workspace_id = $1::uuid AND issue_id = $2::uuid AND status = 'open'`, workspaceID, issueID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoOpenRun
	}
	return r, err
}

func metricsWorkflowsOf(ctx context.Context, q Querier, workspaceID string) ([]MetricsWorkflow, error) {
	rows, err := q.Query(ctx, `SELECT id::text, name, steps FROM issue_workflow WHERE workspace_id = $1::uuid`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsWorkflow{}
	for rows.Next() {
		var wf MetricsWorkflow
		var raw []byte
		if err := rows.Scan(&wf.ID, &wf.Name, &raw); err != nil {
			return nil, err
		}
		// A workflow whose steps do not decode is still listed, without the
		// step-defined parts (way back, step order).
		wf.Steps, _ = issueworkflow.DecodeSteps(raw)
		out = append(out, wf)
	}
	return out, rows.Err()
}

func metricsStatusesOf(ctx context.Context, q Querier, workspaceID string) (map[string]MetricsStatus, error) {
	rows, err := q.Query(ctx, `SELECT key, name, category FROM issue_status WHERE workspace_id = $1::uuid ORDER BY position, key`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MetricsStatus{}
	for rows.Next() {
		var key string
		var s MetricsStatus
		if err := rows.Scan(&key, &s.Name, &s.Category); err != nil {
			return nil, err
		}
		out[key] = s
	}
	return out, rows.Err()
}

// metricsStatusChanges reads the status changes in [from, until) of issues in
// projects, as workflowStatusChanges does for one workflow: each project
// resolves to its workflow, or to the stored Default when it has none or a
// dangling one ("" when the workspace never stored a Default). Dwell is the
// time since the issue's previous status change, or since it was created.
func metricsStatusChanges(ctx context.Context, q Querier, workspaceID string, from, until time.Time,
	hidden []pgtype.UUID, excludeProjects []string) ([]MetricsChange, error) {
	rows, err := q.Query(ctx, `
		WITH def AS (
			SELECT id FROM issue_workflow WHERE workspace_id = $1::uuid AND is_default ORDER BY created_at, id LIMIT 1
		), issues AS (
			SELECT i.id, i.created_at,
			       COALESCE((SELECT o.id FROM issue_workflow o WHERE o.id = p.workflow_id AND o.workspace_id = p.workspace_id),
			                (SELECT id FROM def)) AS workflow_id
			FROM issue i
			JOIN project p ON p.id = i.project_id AND p.workspace_id = i.workspace_id
			WHERE i.workspace_id = $1::uuid
			  AND ($4::uuid[] IS NULL OR NOT (i.project_id = ANY($4::uuid[])))
			  AND NOT (i.project_id = ANY(COALESCE($5::uuid[], '{}')))
		), changes AS (
			SELECT a.issue_id, a.created_at, a.details->>'from' AS from_key, a.details->>'to' AS to_key,
			       COALESCE(lag(a.created_at) OVER (PARTITION BY a.issue_id ORDER BY a.created_at, a.id), issues.created_at) AS entered_at
			FROM activity_log a JOIN issues ON issues.id = a.issue_id
			WHERE a.workspace_id = $1::uuid AND a.action = 'status_changed' AND a.created_at < $3
		)
		SELECT c.issue_id::text, COALESCE(issues.workflow_id::text, ''), COALESCE(c.from_key, ''), COALESCE(c.to_key, ''),
		       EXTRACT(EPOCH FROM (c.created_at - c.entered_at))::float8
		FROM changes c JOIN issues ON issues.id = c.issue_id
		WHERE c.created_at >= $2`, workspaceID, from, until, hidden, excludeProjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsChange{}
	for rows.Next() {
		var c MetricsChange
		if err := rows.Scan(&c.IssueID, &c.WorkflowID, &c.From, &c.To, &c.DwellSecs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func fillLoopIdentifiers(ctx context.Context, q Querier, loops []ReturnLoop) error {
	if len(loops) == 0 {
		return nil
	}
	ids := make([]string, len(loops))
	for i, l := range loops {
		ids[i] = l.IssueID
	}
	rows, err := q.Query(ctx, `
		SELECT i.id::text, w.issue_prefix || '-' || i.number::text
		FROM issue i JOIN workspace w ON w.id = i.workspace_id
		WHERE i.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	identifiers := map[string]string{}
	for rows.Next() {
		var id, identifier string
		if err := rows.Scan(&id, &identifier); err != nil {
			return err
		}
		identifiers[id] = identifier
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range loops {
		loops[i].Identifier = identifiers[loops[i].IssueID]
	}
	return nil
}

// metricsAgentsOf lists the agents a proposal may name: every member can see
// them (workspaceVisibleAgent, as the condense review), they are not internal
// system agents, and they are not excluded. Archived ones are included, since
// their runs in the window still happened. The summary is read in an issue
// every member can open, so a private agent never appears in it.
func metricsAgentsOf(ctx context.Context, q Querier, workspaceID string, excluded []string) ([]MetricsAgent, error) {
	rows, err := q.Query(ctx, `
		SELECT a.id::text, a.name, a.archived_at IS NOT NULL
		FROM agent a
		WHERE a.workspace_id = $1::uuid AND a.kind <> 'system'
		  AND NOT (a.id = ANY(COALESCE($2::uuid[], '{}')))
		  AND `+workspaceVisibleAgent+`
		ORDER BY a.name, a.id`, workspaceID, excluded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsAgent{}
	for rows.Next() {
		var a MetricsAgent
		if err := rows.Scan(&a.ID, &a.Name, &a.Archived); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// metricsTasks reads the agents' runs queued in [from, until). Runs on
// issues in hidden or excluded projects, and runs for chats filed under
// them, are left out.
func metricsTasks(ctx context.Context, q Querier, agentIDs []string, from, until time.Time,
	hidden []pgtype.UUID, excludeProjects []string) ([]MetricsTask, error) {
	if len(agentIDs) == 0 {
		return []MetricsTask{}, nil
	}
	rows, err := q.Query(ctx, `
		SELECT t.agent_id::text, t.status,
		       CASE WHEN t.chat_session_id IS NOT NULL THEN 'chat'
		            WHEN t.autopilot_run_id IS NOT NULL THEN 'autopilot'
		            WHEN t.trigger_comment_id IS NOT NULL THEN 'comment'
		            ELSE COALESCE(NULLIF(t.trigger_evidence_kind, ''), 'other') END,
		       CASE WHEN t.status = 'completed' AND t.started_at IS NOT NULL AND t.completed_at IS NOT NULL
		            THEN EXTRACT(EPOCH FROM (t.completed_at - t.started_at))::float8 END
		FROM agent_task_queue t
		LEFT JOIN issue i ON i.id = t.issue_id
		LEFT JOIN chat_session s ON s.id = t.chat_session_id
		WHERE t.agent_id = ANY($1::uuid[]) AND t.created_at >= $2 AND t.created_at < $3
		  AND (i.project_id IS NULL OR (($4::uuid[] IS NULL OR NOT (i.project_id = ANY($4::uuid[])))
		       AND NOT (i.project_id = ANY(COALESCE($5::uuid[], '{}')))))
		  AND (s.project_id IS NULL OR (($4::uuid[] IS NULL OR NOT (s.project_id = ANY($4::uuid[])))
		       AND NOT (s.project_id = ANY(COALESCE($5::uuid[], '{}')))))`,
		agentIDs, from, until, hidden, excludeProjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsTask{}
	for rows.Next() {
		var t MetricsTask
		if err := rows.Scan(&t.AgentID, &t.Status, &t.Trigger, &t.DurationSecs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReadHandoffs reads the issue handoffs in [from, until), except on issues in
// hidden or excluded projects. A handoff is either row the activity listener
// writes for it: an assignee_changed row (to_*), or, when entering a
// workflow step moved the issue to the step's handler, the status_changed
// row carrying handoff_to_* (the listener then writes no assignee row, so
// nothing is counted twice).
//
// Who the issue came from is the row's own record when it has one
// (router_from_*, then from_* on an assignee row, then handoff_from_*);
// otherwise, as for a workflow handoff written while the Router's
// annotation is off, it is whom the issue's previous handoff row gave it to.
func ReadHandoffs(ctx context.Context, q Querier, workspaceID string, from, until time.Time,
	hidden []pgtype.UUID, excludeProjects []string) ([]MetricsHandoff, error) {
	rows, err := q.Query(ctx, `
		WITH handoffs AS (
			SELECT a.id, a.issue_id, a.created_at,
			       CASE WHEN a.action = 'assignee_changed' THEN a.details->>'to_type' ELSE a.details->>'handoff_to_type' END AS to_type,
			       CASE WHEN a.action = 'assignee_changed' THEN a.details->>'to_id' ELSE a.details->>'handoff_to_id' END AS to_id,
			       CASE WHEN a.details ? 'router_from_type' THEN 'router'
			            WHEN a.action = 'assignee_changed' AND a.details ? 'from_type' THEN 'from'
			            WHEN a.details ? 'handoff_from_type' THEN 'handoff' END AS own_from,
			       a.details
			FROM activity_log a
			JOIN issue i ON i.id = a.issue_id AND i.workspace_id = a.workspace_id
			WHERE a.workspace_id = $1::uuid AND a.created_at < $3
			  AND (a.action = 'assignee_changed' OR (a.action = 'status_changed' AND a.details ? 'handoff_to_type'))
			  AND (i.project_id IS NULL OR (($4::uuid[] IS NULL OR NOT (i.project_id = ANY($4::uuid[])))
			       AND NOT (i.project_id = ANY(COALESCE($5::uuid[], '{}')))))
		), ordered AS (
			SELECT h.*,
			       lag(h.to_type) OVER (PARTITION BY h.issue_id ORDER BY h.created_at, h.id) AS prev_type,
			       lag(h.to_id) OVER (PARTITION BY h.issue_id ORDER BY h.created_at, h.id) AS prev_id
			FROM handoffs h
		)
		SELECT o.issue_id::text,
		       COALESCE(CASE o.own_from WHEN 'router' THEN o.details->>'router_from_type'
		                                WHEN 'from' THEN o.details->>'from_type'
		                                WHEN 'handoff' THEN o.details->>'handoff_from_type'
		                                ELSE o.prev_type END, ''),
		       COALESCE(CASE o.own_from WHEN 'router' THEN o.details->>'router_from_id'
		                                WHEN 'from' THEN o.details->>'from_id'
		                                WHEN 'handoff' THEN o.details->>'handoff_from_id'
		                                ELSE o.prev_id END, ''),
		       COALESCE(o.to_type, ''), COALESCE(o.to_id, '')
		FROM ordered o
		WHERE o.created_at >= $2
		ORDER BY o.created_at, o.id`,
		workspaceID, from, until, hidden, excludeProjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsHandoff{}
	for rows.Next() {
		var h MetricsHandoff
		if err := rows.Scan(&h.IssueID, &h.FromType, &h.FromID, &h.ToType, &h.ToID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
