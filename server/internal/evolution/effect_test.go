// Added by the mouse9527/multica fork: Evolution P5 (M-139).

package evolution

import (
	"strings"
	"testing"
)

func findingOf(t *testing.T, findings []Finding, metric string) Finding {
	t.Helper()
	for _, f := range findings {
		if f.Metric == metric {
			return f
		}
	}
	t.Fatalf("no finding for %s in %+v", metric, findings)
	return Finding{}
}

// sampled gives every metric in m a sample of n.
func sampled(m Metrics, n int) Metrics {
	out := Metrics{}
	for k, v := range m {
		out[k] = v
		out[metricSamplePrefix+k] = float64(n)
	}
	return out
}

func TestCompareMetrics(t *testing.T) {
	before := sampled(Metrics{MetricFailureRate: 0.10, MetricRetryRate: 0.02, MetricAvgDurationSecs: 600}, 10)
	after := sampled(Metrics{MetricFailureRate: 0.20, MetricRetryRate: 0.04, MetricAvgDurationSecs: 300}, 10)
	got := CompareMetrics(TargetAgent, before, after, 20, 5)
	if f := findingOf(t, got, MetricFailureRate); f.Direction != DirectionWorse {
		t.Errorf("failure rate 10%% → 20%%: %+v", f)
	}
	// Doubled, but by two points: below the absolute floor.
	if f := findingOf(t, got, MetricRetryRate); f.Direction != DirectionSame {
		t.Errorf("retry rate 2%% → 4%%: %+v", f)
	}
	if f := findingOf(t, got, MetricAvgDurationSecs); f.Direction != DirectionBetter {
		t.Errorf("duration 600s → 300s: %+v", f)
	}
	// Within the relative threshold.
	if f := findingOf(t, CompareMetrics(TargetAgent, sampled(Metrics{MetricFailureRate: 0.50}, 10), sampled(Metrics{MetricFailureRate: 0.58}, 10), 20, 5), MetricFailureRate); f.Direction != DirectionSame {
		t.Errorf("50%% → 58%% at a 20%% threshold: %+v", f)
	}
	// A metric one window lacks is not compared.
	if got := CompareMetrics(TargetAgent, sampled(Metrics{MetricFailureRate: 0.1}, 10), Metrics{}, 20, 5); len(got) != 0 {
		t.Errorf("compared a missing metric: %+v", got)
	}
	// Per-step dwell is recorded, never judged.
	wf := CompareMetrics(TargetWorkflow, sampled(Metrics{metricStepDwellPrefix + "in_review": 1}, 10), sampled(Metrics{metricStepDwellPrefix + "in_review": 50}, 10), 20, 5)
	if len(wf) != 0 {
		t.Errorf("judged per-step dwell: %+v", wf)
	}
	// A metric below the sample minimum in either window is not compared.
	if got := CompareMetrics(TargetAgent, sampled(Metrics{MetricFailureRate: 0.1}, 4), sampled(Metrics{MetricFailureRate: 0.9}, 10), 20, 5); len(got) != 0 {
		t.Errorf("compared a metric with 4 samples before: %+v", got)
	}
	if got := CompareMetrics(TargetAgent, Metrics{MetricFailureRate: 0.1}, Metrics{MetricFailureRate: 0.9}, 20, 5); len(got) != 0 {
		t.Errorf("compared a metric without a recorded sample: %+v", got)
	}
}

// TestDurationNeedsItsOwnSample (review R1): ten finished runs per window
// pass the run minimum, but only one completed run per window was timed, so
// its tenfold duration is not judged and nothing regressed.
func TestDurationNeedsItsOwnSample(t *testing.T) {
	before, nb := AgentMetrics(AgentTaskCounts{Finished: 10, Failed: 9, AvgDurationSecs: 60, Timed: 1})
	after, na := AgentMetrics(AgentTaskCounts{Finished: 10, Failed: 9, AvgDurationSecs: 600, Timed: 1})
	findings := CompareMetrics(TargetAgent, before, after, 20, 5)
	for _, f := range findings {
		if f.Metric == MetricAvgDurationSecs {
			t.Fatalf("judged the duration of one run: %+v", f)
		}
	}
	if got := Verdict(nb, na, 5, findings); got != EffectUnchanged {
		t.Fatalf("verdict = %s, want %s", got, EffectUnchanged)
	}
	// With enough timed runs, the same slowdown is a regression.
	before, _ = AgentMetrics(AgentTaskCounts{Finished: 10, Failed: 4, AvgDurationSecs: 60, Timed: 6})
	after, _ = AgentMetrics(AgentTaskCounts{Finished: 10, Failed: 4, AvgDurationSecs: 600, Timed: 6})
	if got := Verdict(10, 10, 5, CompareMetrics(TargetAgent, before, after, 20, 5)); got != EffectRegressed {
		t.Fatalf("verdict with six timed runs = %s, want %s", got, EffectRegressed)
	}
}

func TestVerdict(t *testing.T) {
	worse := []Finding{{Metric: MetricFailureRate, Direction: DirectionWorse}, {Metric: MetricRetryRate, Direction: DirectionBetter}}
	better := []Finding{{Metric: MetricFailureRate, Direction: DirectionBetter}, {Metric: MetricRetryRate, Direction: DirectionSame}}
	same := []Finding{{Metric: MetricFailureRate, Direction: DirectionSame}}
	cases := []struct {
		name          string
		before, after int
		findings      []Finding
		want          string
	}{
		{"few runs before", 4, 20, worse, EffectInsufficient},
		{"few runs after", 20, 4, worse, EffectInsufficient},
		{"any worse is a regression", 5, 5, worse, EffectRegressed},
		{"better and none worse", 10, 10, better, EffectImproved},
		{"nothing moved", 10, 10, same, EffectUnchanged},
		// No metric had enough samples of its own.
		{"nothing to compare", 10, 10, nil, EffectInsufficient},
	}
	for _, tc := range cases {
		if got := Verdict(tc.before, tc.after, 5, tc.findings); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestAgentMetrics(t *testing.T) {
	m, n := AgentMetrics(AgentTaskCounts{Finished: 8, Failed: 2, Retried: 1, AvgDurationSecs: 120.5, Timed: 6})
	if n != 8 || m[MetricFailureRate] != 0.25 || m[MetricRetryRate] != 0.125 || m[MetricAvgDurationSecs] != 120.5 {
		t.Fatalf("got %v, %d", m, n)
	}
	if m[metricSamplePrefix+MetricFailureRate] != 8 || m[metricSamplePrefix+MetricRetryRate] != 8 || m[metricSamplePrefix+MetricAvgDurationSecs] != 6 {
		t.Fatalf("samples: %v", m)
	}
	if m, n := AgentMetrics(AgentTaskCounts{}); n != 0 || len(m) != 0 {
		t.Fatalf("no runs: %v, %d", m, n)
	}
}

const reviewSteps = `[
	{"status_key": "todo", "handler": {"type": "agent", "id": "a"}},
	{"status_key": "in_review", "handler": {"type": "agent", "id": "b"}, "back_status_key": "todo"},
	{"status_key": "started_2"},
	{"status_key": "started_6", "handler": {"type": "agent", "id": "a"}},
	{"status_key": "done"}
]`

// TestWorkflowReturns: a return is a move from a step to exactly its
// back_status_key. Moving back anywhere else, say from "awaiting decision" to
// "execute", is not one.
func TestWorkflowReturns(t *testing.T) {
	returns := ReturnsOf(reviewSteps)
	if len(returns) != 1 || returns["in_review"] != "todo" {
		t.Fatalf("returns = %v", returns)
	}
	changes := []StatusChange{
		{IssueID: "1", From: "todo", To: "in_review", FromCategory: "todo", DwellSecs: 3600},
		{IssueID: "1", From: "in_review", To: "todo", FromCategory: "in_review", DwellSecs: 7200},
		{IssueID: "1", From: "todo", To: "in_review", FromCategory: "todo", DwellSecs: 3600},
		{IssueID: "2", From: "started_2", To: "started_6", FromCategory: "in_progress", DwellSecs: 3600},
		{IssueID: "2", From: "done", To: "todo", FromCategory: "done", DwellSecs: 360000},
	}
	m, n := WorkflowMetrics(changes, returns)
	if n != 2 {
		t.Fatalf("sample = %d, want the two issues that moved", n)
	}
	if m[MetricReturnsPerIssue] != 0.5 {
		t.Errorf("returns per issue = %v, want 1 return over 2 issues", m[MetricReturnsPerIssue])
	}
	// Time sitting in done is not dwell: (1+2+1+1)/4 hours.
	if m[MetricAvgDwellHours] != 1.25 {
		t.Errorf("avg dwell = %v", m[MetricAvgDwellHours])
	}
	if m[metricStepDwellPrefix+"in_review"] != 2 || m[metricStepDwellPrefix+"todo"] != 1 {
		t.Errorf("per-step dwell: %v", m)
	}
	if _, ok := m[metricStepDwellPrefix+"done"]; ok {
		t.Error("dwell recorded for done")
	}
	if m[metricSamplePrefix+MetricReturnsPerIssue] != 2 || m[metricSamplePrefix+MetricAvgDwellHours] != 2 {
		t.Errorf("samples: %v", m)
	}
	// Dwell counted for one issue only: its sample is that one issue, while
	// returns still count both.
	m, _ = WorkflowMetrics([]StatusChange{
		{IssueID: "1", From: "in_review", To: "todo", FromCategory: "in_review", DwellSecs: 3600},
		{IssueID: "2", From: "done", To: "todo", FromCategory: "done", DwellSecs: 3600},
	}, returns)
	if m[metricSamplePrefix+MetricAvgDwellHours] != 1 || m[metricSamplePrefix+MetricReturnsPerIssue] != 2 {
		t.Errorf("samples: %v", m)
	}

	// A workflow without a way back does not measure returns.
	m, _ = WorkflowMetrics(changes, ReturnsOf(`[{"status_key": "todo"}, {"status_key": "done"}]`))
	if _, ok := m[MetricReturnsPerIssue]; ok {
		t.Errorf("returns measured without a back step: %v", m)
	}
	if got := ReturnsOf("not json"); got != nil {
		t.Errorf("unreadable steps: %v", got)
	}
}

func TestEffectNotice(t *testing.T) {
	n := EffectNotice{
		AgentName: "进化", TargetType: TargetAgent, TargetName: "开发", Field: "instructions", Version: 3,
		WindowDays: 7, SampleBefore: 12, SampleAfter: 9, ThresholdPercent: 20,
		Findings: []Finding{
			{Metric: MetricFailureRate, Before: 0.1, After: 0.4, Direction: DirectionWorse},
			{Metric: MetricAvgDurationSecs, Before: 600, After: 300, Direction: DirectionBetter},
		},
	}
	body := n.Body()
	for _, want := range []string{"建议回滚", "第 3 版", "运行失败率", "10.0%", "40.0%", "应用前 12 次运行，应用后 9", "没有自动回滚"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "平均运行时长") {
		t.Errorf("an improved metric is listed:\n%s", body)
	}
	if !strings.Contains(n.Title(), "智能体 开发") {
		t.Errorf("title: %s", n.Title())
	}
	n.AgentName, n.TargetType = "Evolution", TargetWorkflow
	n.Findings = []Finding{{Metric: MetricReturnsPerIssue, Before: 0.2, After: 0.6, Direction: DirectionWorse}}
	if en := n.Body(); !strings.Contains(en, "Suggest rolling back") || !strings.Contains(en, "Returns per issue") || !strings.Contains(en, "12 issues before") {
		t.Errorf("English body:\n%s", en)
	}
}

func TestConfigEffectDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EffectWindowDays != 7 {
		t.Fatalf("effect window default = %d, want 7", cfg.EffectWindowDays)
	}
	cfg.EffectWindowDays, cfg.RegressionThresholdPercent, cfg.MinEffectSample = 0, 0, 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a client without the P5 fields must still save: %v", err)
	}
	if n := cfg.normalized(); n.EffectWindowDays != DefaultEffectWindowDays || n.RegressionThresholdPercent != DefaultRegressionThresholdPercent || n.MinEffectSample != DefaultMinEffectSample {
		t.Errorf("defaults not filled: %+v", n)
	}
	cfg.MinEffectSample = MaxEffectSampleLowerBound + 1
	if !IsInvalidConfig(cfg.Validate()) {
		t.Error("over the bound must be rejected")
	}
	stored := ConfigFromSettings([]byte(`{"evolution": {"effect_window_days": 90, "regression_threshold_percent": 1}}`))
	if stored.EffectWindowDays != MaxEffectWindowDays || stored.RegressionThresholdPercent != MinRegressionThresholdPercent || stored.MinEffectSample != DefaultMinEffectSample {
		t.Errorf("stored values not clamped: %+v", stored)
	}
}
