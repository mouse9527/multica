// Added by the mouse9527/multica fork: Evolution structural analysis, the
// metrics source (M-296).

package evolution

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// deliveryWorkflow is the shape of the delivery workflow the M-291 samples
// came from: review sends back to todo, the second review back to review,
// acceptance back to todo; the decision column has no way back.
func deliveryWorkflow() MetricsWorkflow {
	return MetricsWorkflow{ID: "wf-delivery", Name: "delivery", Steps: []issueworkflow.Step{
		{StatusKey: "todo"},
		{StatusKey: "in_progress"},
		{StatusKey: "in_review", BackStatusKey: "todo"},
		{StatusKey: "second_review", BackStatusKey: "in_review"},
		{StatusKey: "acceptance", BackStatusKey: "todo"},
		{StatusKey: "decide"},
		{StatusKey: "done"},
	}}
}

func deliveryStatuses() map[string]MetricsStatus {
	return map[string]MetricsStatus{
		"todo":          {Name: "Todo", Category: "todo"},
		"in_progress":   {Name: "In progress", Category: "in_progress"},
		"in_review":     {Name: "In review", Category: "in_progress"},
		"second_review": {Name: "Second review", Category: "in_progress"},
		"acceptance":    {Name: "Acceptance", Category: "in_progress"},
		"decide":        {Name: "Decide", Category: "in_progress"},
		"done":          {Name: "Done", Category: "done"},
	}
}

func change(issue, from, to string, dwellHours float64) MetricsChange {
	return MetricsChange{WorkflowID: "wf-delivery", StatusChange: StatusChange{
		IssueID: issue, From: from, To: to, DwellSecs: dwellHours * 3600,
	}}
}

func stepOf(t *testing.T, wf WorkflowSummary, key string) StepSummary {
	t.Helper()
	for _, s := range wf.Steps {
		if s.StatusKey == key {
			return s
		}
	}
	t.Fatalf("step %s missing from %+v", key, wf.Steps)
	return StepSummary{}
}

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: missing, want %v", name, want)
	}
	if d := *got - want; d > 0.0001 || d < -0.0001 {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}

// TestSummarizeWorkflowsReviewReturnLoop is M-291 sample 1: review sends
// work back to todo over and over, one issue many times. The summary carries
// the review step's return rate and lists the issue as a return loop.
func TestSummarizeWorkflowsReviewReturnLoop(t *testing.T) {
	changes := []MetricsChange{}
	// Issue A: returned from review three times, then accepted.
	for i := 0; i < 3; i++ {
		changes = append(changes,
			change("A", "in_progress", "in_review", 1),
			change("A", "in_review", "todo", 2),
			change("A", "todo", "in_progress", 0.5))
	}
	changes = append(changes, change("A", "in_progress", "in_review", 1), change("A", "in_review", "acceptance", 2))
	// Issue B: passes review once.
	changes = append(changes, change("B", "in_progress", "in_review", 1), change("B", "in_review", "acceptance", 1))
	// Issue C: returned twice: not a loop.
	changes = append(changes,
		change("C", "in_review", "todo", 1), change("C", "todo", "in_review", 1), change("C", "in_review", "todo", 1))

	wfs, loops := SummarizeWorkflows(changes, []MetricsWorkflow{deliveryWorkflow()}, deliveryStatuses())
	if len(wfs) != 1 || wfs[0].ID != "wf-delivery" || wfs[0].Name != "delivery" || wfs[0].Issues != 3 {
		t.Fatalf("workflows = %+v", wfs)
	}
	review := stepOf(t, wfs[0], "in_review")
	if review.Exits != 7 || review.Returns != 5 || review.ExitsTo["todo"] != 5 || review.ExitsTo["acceptance"] != 2 {
		t.Fatalf("review = %+v", review)
	}
	if review.BackStatusKey != "todo" || !review.InWorkflow || review.StatusName != "In review" {
		t.Fatalf("review step metadata = %+v", review)
	}
	approx(t, "review return rate", review.ReturnRate, 5.0/7)
	if len(loops) != 1 || loops[0].IssueID != "A" || loops[0].Returns != 3 || loops[0].ByStep["in_review"] != 3 {
		t.Fatalf("loops = %+v", loops)
	}
	// A step without a way back has no return rate.
	if s := stepOf(t, wfs[0], "todo"); s.ReturnRate != nil || s.Returns != 0 {
		t.Fatalf("todo = %+v", s)
	}
	// Steps come in the workflow's order.
	var keys []string
	for _, s := range wfs[0].Steps {
		keys = append(keys, s.StatusKey)
	}
	want := []string{"todo", "in_progress", "in_review", "acceptance"}
	if len(keys) != len(want) {
		t.Fatalf("step order = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("step order = %v, want %v", keys, want)
		}
	}
}

// TestSummarizeWorkflowsDecisionDwell is M-291 sample 2: work waits long in
// the decision column. The summary carries its entries by source and the
// median and p90 dwell; resting categories (done) get no dwell.
func TestSummarizeWorkflowsDecisionDwell(t *testing.T) {
	changes := []MetricsChange{
		change("A", "acceptance", "decide", 1),
		change("B", "acceptance", "decide", 1),
		change("C", "acceptance", "decide", 1),
		change("D", "in_review", "decide", 1),
		change("A", "decide", "done", 1),
		change("B", "decide", "done", 2),
		change("C", "decide", "done", 3),
		change("D", "decide", "todo", 63),
		change("A", "done", "todo", 500),
	}
	wfs, loops := SummarizeWorkflows(changes, []MetricsWorkflow{deliveryWorkflow()}, deliveryStatuses())
	if len(loops) != 0 {
		t.Fatalf("loops = %+v", loops)
	}
	decide := stepOf(t, wfs[0], "decide")
	if decide.Entries != 4 || decide.Exits != 4 || decide.DwellSample != 4 {
		t.Fatalf("decide = %+v", decide)
	}
	// Sorted 1, 2, 3, 63: p50 = 2.5, p90 = 3 + 0.7*60 = 45.
	approx(t, "decide p50", decide.DwellP50Hours, 2.5)
	approx(t, "decide p90", decide.DwellP90Hours, 45)
	if acc := stepOf(t, wfs[0], "acceptance"); acc.ExitsTo["decide"] != 3 {
		t.Fatalf("acceptance = %+v", acc)
	}
	if done := stepOf(t, wfs[0], "done"); done.DwellSample != 0 || done.DwellP50Hours != nil || done.Exits != 1 {
		t.Fatalf("done = %+v", done)
	}
}

// TestSummarizeSecondReviewYield is M-291 sample 3: the second review passes
// almost everything on; its return rate and the reviewer's run time are what
// a proposal to narrow it would cite.
func TestSummarizeSecondReviewYield(t *testing.T) {
	changes := []MetricsChange{}
	for _, issue := range []string{"A", "B", "C", "D", "E", "F"} {
		changes = append(changes, change(issue, "in_review", "second_review", 1), change(issue, "second_review", "acceptance", 2))
	}
	changes = append(changes, change("G", "in_review", "second_review", 1), change("G", "second_review", "in_review", 2))
	wfs, _ := SummarizeWorkflows(changes, []MetricsWorkflow{deliveryWorkflow()}, deliveryStatuses())
	second := stepOf(t, wfs[0], "second_review")
	if second.Entries != 7 || second.Exits != 7 || second.Returns != 1 || second.ExitsTo["acceptance"] != 6 {
		t.Fatalf("second review = %+v", second)
	}
	approx(t, "second review return rate", second.ReturnRate, 1.0/7)

	secs := func(v float64) *float64 { return &v }
	tasks := []MetricsTask{}
	for _, d := range []float64{300, 340, 362, 400, 420} {
		tasks = append(tasks, MetricsTask{AgentID: "reviewer-2", Status: "completed", Trigger: "issue_assignment", DurationSecs: secs(d)})
	}
	agents := SummarizeAgents([]MetricsAgent{{ID: "reviewer-2", Name: "Reviewer 2"}}, tasks, nil)
	approx(t, "reviewer-2 p50", agents[0].DurationP50Secs, 362)
	if agents[0].Runs != 5 || agents[0].Triggers["issue_assignment"] != 5 {
		t.Fatalf("reviewer-2 = %+v", agents[0])
	}
}

// TestSummarizeWorkflowsUnknownWorkflow: a change whose workflow is not
// stored (the built-in Default) is grouped on its own, named Default, with
// no returns measured.
func TestSummarizeWorkflowsUnknownWorkflow(t *testing.T) {
	c := change("A", "in_review", "todo", 1)
	c.WorkflowID = ""
	wfs, loops := SummarizeWorkflows([]MetricsChange{c}, nil, deliveryStatuses())
	if len(wfs) != 1 || wfs[0].ID != "" || wfs[0].Name != issueworkflow.DefaultName || len(loops) != 0 {
		t.Fatalf("workflows = %+v loops = %+v", wfs, loops)
	}
	if s := stepOf(t, wfs[0], "in_review"); s.ReturnRate != nil || s.InWorkflow {
		t.Fatalf("in_review = %+v", s)
	}
}

// TestSummarizeAgents covers the per-agent numbers: rates over finished
// runs, the median over completed timed runs, short runs, triggers, and
// handoffs in both directions (not to itself, not for agents left out).
func TestSummarizeAgents(t *testing.T) {
	secs := func(v float64) *float64 { return &v }
	tasks := []MetricsTask{
		{AgentID: "a", Status: "completed", Trigger: "chat", DurationSecs: secs(10)},
		{AgentID: "a", Status: "completed", Trigger: "chat", DurationSecs: secs(30)},
		{AgentID: "a", Status: "completed", Trigger: "comment", DurationSecs: secs(100)},
		{AgentID: "a", Status: "completed", Trigger: "autopilot"},
		{AgentID: "a", Status: "failed", Trigger: "autopilot"},
		{AgentID: "a", Status: "cancelled", Trigger: ""},
		{AgentID: "a", Status: "running", Trigger: "issue_assignment"},
		{AgentID: "hidden", Status: "completed", Trigger: "chat", DurationSecs: secs(5)},
	}
	handoffs := []MetricsHandoff{
		{FromType: "agent", FromID: "a", ToType: "agent", ToID: "b"},
		{FromType: "member", FromID: "m", ToType: "agent", ToID: "a"},
		{FromType: "agent", FromID: "b", ToType: "agent", ToID: "a"},
		{FromType: "agent", FromID: "a", ToType: "agent", ToID: "a"},
		{FromType: "agent", FromID: "a", ToType: "member", ToID: "m"},
		{FromType: "", FromID: "", ToType: "agent", ToID: "b"},
	}
	got := SummarizeAgents([]MetricsAgent{{ID: "a", Name: "A"}, {ID: "b", Name: "B", Archived: true}}, tasks, handoffs)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("agents = %+v", got)
	}
	a := got[0]
	if a.Runs != 7 || a.Finished != 6 || a.Completed != 4 || a.Failed != 1 || a.Cancelled != 1 {
		t.Fatalf("a counts = %+v", a)
	}
	approx(t, "a failure rate", a.FailureRate, 1.0/6)
	approx(t, "a cancel rate", a.CancelRate, 1.0/6)
	if a.DurationSample != 3 || a.ShortRuns != 2 {
		t.Fatalf("a durations = %+v", a)
	}
	approx(t, "a p50", a.DurationP50Secs, 30)
	approx(t, "a short run rate", a.ShortRunRate, 2.0/3)
	if a.Triggers["chat"] != 2 || a.Triggers["comment"] != 1 || a.Triggers["autopilot"] != 2 ||
		a.Triggers[TriggerOther] != 1 || a.Triggers["issue_assignment"] != 1 {
		t.Fatalf("a triggers = %+v", a.Triggers)
	}
	if a.HandoffsOut != 2 || a.HandoffsIn != 2 {
		t.Fatalf("a handoffs = in %d out %d", a.HandoffsIn, a.HandoffsOut)
	}
	b := got[1]
	if b.Runs != 0 || b.FailureRate != nil || b.DurationP50Secs != nil || !b.Archived || b.HandoffsIn != 2 || b.HandoffsOut != 1 {
		t.Fatalf("b = %+v", b)
	}
}

func TestPercentile(t *testing.T) {
	cases := []struct {
		in   []float64
		p    float64
		want float64
	}{
		{[]float64{7}, 0.9, 7},
		{[]float64{1, 2}, 0.5, 1.5},
		{[]float64{1, 2, 3}, 0.5, 2},
		{[]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.9, 9.1},
	}
	for _, c := range cases {
		if got := percentile(c.in, c.p); got-c.want > 1e-9 || c.want-got > 1e-9 {
			t.Errorf("percentile(%v, %v) = %v, want %v", c.in, c.p, got, c.want)
		}
	}
}

func TestMetricsWantedNeedsTheSourceAndTheSchedule(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Sources.Metrics || cfg.MetricsWanted() {
		t.Fatal("metrics must be off by default")
	}
	cfg.Sources.Metrics = true
	if cfg.MetricsWanted() {
		t.Fatal("metrics must stay off while the scheduled analysis is off")
	}
	cfg.Enabled = true
	if !cfg.MetricsWanted() {
		t.Fatal("metrics should be wanted")
	}
	stored := ConfigFromSettings([]byte(`{"evolution": {"enabled": true, "sources": {"comments": true}}}`))
	if stored.Sources.Metrics {
		t.Fatal("a stored config without the field must read as off")
	}
}
