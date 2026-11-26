// Added by the mouse9527/multica fork: Evolution structural analysis, metric
// evidence (M-296).

package evolution

import (
	"strings"
	"testing"
	"time"
)

const (
	testWorkflowID = "0195f0a0-0000-7000-8000-0000000000aa"
	testReviewerID = "0195f0a0-0000-7000-8000-0000000000bb"
	testLoopIssue  = "0195f0a0-0000-7000-8000-0000000000cc"
)

func TestParseMetricRef(t *testing.T) {
	for in, want := range map[string]string{
		"agent:" + testReviewerID + "/failure_rate":                  "agent:" + testReviewerID + "/failure_rate",
		"agent:" + strings.ToUpper(testReviewerID) + "/runs":         "agent:" + testReviewerID + "/runs",
		"workflow:" + testWorkflowID + "/step:in_review/return_rate": "workflow:" + testWorkflowID + "/step:in_review/return_rate",
		"workflow:default/step:started_2/dwell_p90_hours":            "workflow:default/step:started_2/dwell_p90_hours",
		"workflow:" + testWorkflowID + "/issues":                     "workflow:" + testWorkflowID + "/issues",
		"issue:" + testLoopIssue + "/returns":                        "issue:" + testLoopIssue + "/returns",
	} {
		ref, err := ParseMetricRef(in)
		if err != nil {
			t.Errorf("ParseMetricRef(%q): %v", in, err)
			continue
		}
		if ref.String() != want {
			t.Errorf("ParseMetricRef(%q) = %q, want %q", in, ref, want)
		}
	}
	for _, in := range []string{
		"",
		"failure_rate",
		"agent:not-a-uuid/runs",
		"agent:" + testReviewerID + "/tokens",
		"agent:" + testReviewerID,
		"workflow:" + testWorkflowID + "/step:/exits",
		"workflow:" + testWorkflowID + "/step:in_review/latency",
		"workflow:" + testWorkflowID + "/returns",
		"issue:" + testLoopIssue + "/dwell",
		"agent:default/runs",
		"squad:" + testReviewerID + "/runs",
	} {
		if _, err := ParseMetricRef(in); err == nil {
			t.Errorf("ParseMetricRef(%q) accepted", in)
		}
	}
}

// structuralSummary holds the numbers behind M-291's samples 1 and 3: review
// sends 15 of 54 exits back; the second reviewer has 8 finished runs, 2
// failed, with a p50 of 362 s; one issue was returned 4 times.
func structuralSummary() MetricsSummary {
	rate := func(v float64) *float64 { return &v }
	until := time.Date(2026, 11, 30, 2, 55, 0, 0, time.UTC)
	return MetricsSummary{
		Source: SourceMetrics, WindowUntil: until, WindowFrom: until.AddDate(0, 0, -14), WindowDays: 14,
		Workflows: []WorkflowSummary{
			{ID: testWorkflowID, Name: "delivery", Issues: 40, Steps: []StepSummary{
				{StatusKey: "in_review", Entries: 60, Exits: 54, Returns: 15, BackStatusKey: "todo", ReturnRate: rate(0.2778),
					DwellSample: 54, DwellP50Hours: rate(1.5), DwellP90Hours: rate(6)},
				{StatusKey: "todo", Entries: 20, Exits: 3},
			}},
			{ID: "", Name: "Default", Issues: 2, Steps: []StepSummary{{StatusKey: "todo", Exits: 2}}},
		},
		Agents: []AgentSummary{
			{ID: testReviewerID, Name: "Reviewer 2", Runs: 9, Finished: 8, Completed: 6, Failed: 2, FailureRate: rate(0.25),
				CancelRate: rate(0), DurationSample: 6, DurationP50Secs: rate(362), ShortRuns: 1, ShortRunRate: rate(0.1667)},
		},
		ReturnLoops: []ReturnLoop{{IssueID: testLoopIssue, WorkflowID: testWorkflowID, Returns: 4}},
	}
}

func checkRef(t *testing.T, s MetricsSummary, ref string, cited float64, minSample int) (MetricReading, error) {
	t.Helper()
	r, err := ParseMetricRef(ref)
	if err != nil {
		t.Fatalf("ParseMetricRef(%q): %v", ref, err)
	}
	return CheckMetric(0, s, r, cited, minSample)
}

// TestCheckMetric: a cited metric passes when the recomputed value matches
// (rounding allowed) on a large enough sample, and is refused, with the real
// value in the reason, when it does not.
func TestCheckMetric(t *testing.T) {
	s := structuralSummary()
	ok := []struct {
		ref    string
		cited  float64
		value  float64
		sample int
	}{
		{"workflow:" + testWorkflowID + "/step:in_review/return_rate", 0.28, 0.2778, 54},
		{"workflow:" + testWorkflowID + "/step:in_review/returns", 15, 15, 54},
		{"workflow:" + testWorkflowID + "/step:in_review/dwell_p90_hours", 6, 6, 54},
		{"workflow:" + testWorkflowID + "/issues", 40, 40, 40},
		{"agent:" + testReviewerID + "/failure_rate", 0.25, 0.25, 8},
		{"agent:" + testReviewerID + "/duration_p50_seconds", 360, 362, 6},
		{"agent:" + testReviewerID + "/short_run_rate", 0.17, 0.1667, 6},
		{"issue:" + testLoopIssue + "/returns", 4, 4, 4},
	}
	for _, c := range ok {
		got, err := checkRef(t, s, c.ref, c.cited, 4)
		if err != nil {
			t.Errorf("%s = %v: %v", c.ref, c.cited, err)
			continue
		}
		if got.Value != c.value || got.Sample != c.sample || !got.WindowUntil.Equal(s.WindowUntil) {
			t.Errorf("%s reading = %+v, want value %v sample %d", c.ref, got, c.value, c.sample)
		}
	}

	refused := []struct {
		name, ref string
		cited     float64
		minSample int
		reason    string
	}{
		{"wrong value", "workflow:" + testWorkflowID + "/step:in_review/return_rate", 0.4, 5, "is 0.2778"},
		{"percent instead of fraction", "agent:" + testReviewerID + "/failure_rate", 25, 5, "is 0.25"},
		{"small sample", "agent:" + testReviewerID + "/failure_rate", 0.25, 10, "fewer than the 10"},
		{"unknown agent", "agent:" + testTargetID + "/runs", 1, 1, "not in the metrics summary"},
		{"unknown step", "workflow:" + testWorkflowID + "/step:done/exits", 1, 1, "no change"},
		{"no way back", "workflow:" + testWorkflowID + "/step:todo/return_rate", 0, 1, "not measured"},
		{"no way back count", "workflow:" + testWorkflowID + "/step:todo/returns", 0, 1, "not measured"},
		{"issue not looping", "issue:" + testIssueID + "/returns", 1, 1, "return loops"},
	}
	for _, c := range refused {
		_, err := checkRef(t, s, c.ref, c.cited, c.minSample)
		if rejectionCode(err) != RejectEvidence || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: err = %v, want %s mentioning %q", c.name, err, RejectEvidence, c.reason)
		}
	}

	// The built-in Default workflow is "default".
	if _, err := checkRef(t, s, "workflow:default/step:todo/exits", 2, 1); err != nil {
		t.Errorf("default workflow: %v", err)
	}
}

func metricProposal() ProposalRequest {
	p := validProposal()
	p.Target = ProposalTarget{Type: TargetTeam, ID: testTargetID}
	p.Field = "retire_agent"
	p.Change = ProposalChange{After: "Retire the second reviewer; review high-risk work in the first review."}
	value := 0.25
	p.Evidence = []ProposalEvidence{
		{Type: EvidenceMetric, ID: "agent:" + strings.ToUpper(testReviewerID) + "/failure_rate", Value: &value, Note: "a quarter of its runs fail"},
		{Type: EvidenceComment, ID: testCommentA},
	}
	return p
}

// TestProposalValidateMetricEvidence: metric items need a parsable id and a
// value, take no quote, and are normalised; value belongs to metrics only;
// retire_agent is a team field.
func TestProposalValidateMetricEvidence(t *testing.T) {
	p := metricProposal()
	if err := p.Validate(); err != nil {
		t.Fatalf("valid metric proposal rejected: %v", err)
	}
	if want := "agent:" + testReviewerID + "/failure_rate"; p.Evidence[0].ID != want {
		t.Fatalf("metric id = %q, want normalised %q", p.Evidence[0].ID, want)
	}
	value := 1.0
	cases := []struct {
		name string
		edit func(p *ProposalRequest)
		code string
	}{
		{"no value", func(p *ProposalRequest) { p.Evidence[0].Value = nil }, RejectInvalid},
		{"bad reference", func(p *ProposalRequest) { p.Evidence[0].ID = "agent:" + testReviewerID + "/tokens" }, RejectInvalid},
		{"quoted metric", func(p *ProposalRequest) { p.Evidence[0].Quote = "anything" }, RejectQuote},
		{"value on a comment", func(p *ProposalRequest) { p.Evidence[1].Value = &value }, RejectInvalid},
		{"same metric twice", func(p *ProposalRequest) { p.Evidence[1] = p.Evidence[0] }, RejectInvalid},
		{"retire on an agent", func(p *ProposalRequest) { p.Target.Type = TargetAgent }, RejectField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := metricProposal()
			tc.edit(&p)
			if got := rejectionCode(p.Validate()); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

// TestRenderMetricEvidence: the proposal shows the server's number, its
// sample, and the window, not only what the agent cited.
func TestRenderMetricEvidence(t *testing.T) {
	p := metricProposal()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	s := structuralSummary()
	ref, _ := ParseMetricRef(p.Evidence[0].ID)
	reading, err := CheckMetric(0, s, ref, *p.Evidence[0].Value, 5)
	if err != nil {
		t.Fatal(err)
	}
	items := []ResolvedEvidence{{ProposalEvidence: p.Evidence[0], CreatedAt: reading.WindowUntil, Context: ref.subject(), Metric: &reading}}
	snap := TargetSnapshot{Type: TargetTeam, ID: testTargetID, Name: "Team", Fields: map[string]string{}}
	for lang, want := range map[string]string{
		"zh": "指标 · `agent:" + testReviewerID + "/failure_rate` · 0.25（样本 8，2026-11-16 至 2026-11-30，服务端重算）",
		"en": "Metric · `agent:" + testReviewerID + "/failure_rate` · 0.25 (sample 8, 2026-11-16 to 2026-11-30, recomputed by the server)",
	} {
		body := renderProposal(lang, &p, snap, items, testRunIssue, "HAN-9", time.UTC)
		if !strings.Contains(body, want) || !strings.Contains(body, "a quarter of its runs fail") {
			t.Errorf("%s body lacks %q:\n%s", lang, want, body)
		}
	}
}

// TestMetricSubjectsAreIndependentSources: numbers of one agent count as one
// source; another agent, a step, or an issue is another.
func TestMetricSubjectsAreIndependentSources(t *testing.T) {
	subject := func(s string) string {
		r, err := ParseMetricRef(s)
		if err != nil {
			t.Fatal(err)
		}
		return r.subject()
	}
	a := "agent:" + testReviewerID
	if subject(a+"/runs") != subject(a+"/failure_rate") {
		t.Error("two numbers of one agent count as two sources")
	}
	step := "workflow:" + testWorkflowID + "/step:in_review"
	if subject(step+"/returns") != subject(step+"/dwell_p50_hours") {
		t.Error("two numbers of one step count as two sources")
	}
	if subject(step+"/returns") == subject("workflow:"+testWorkflowID+"/step:todo/exits") ||
		subject(a+"/runs") == subject("issue:"+testLoopIssue+"/returns") {
		t.Error("different subjects count as one source")
	}
}
