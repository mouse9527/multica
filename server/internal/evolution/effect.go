// Added by the mouse9527/multica fork: Evolution P5, effect measurement
// (M-139).

package evolution

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Effect verdicts, kept in evolution_effect.verdict and in the proposal
// issue's evolution_effect key.
const (
	MetaEffect = "evolution_effect"

	EffectImproved     = "improved"
	EffectUnchanged    = "unchanged"
	EffectRegressed    = "regressed"
	EffectInsufficient = "insufficient_data"
	EffectNotMeasured  = "not_measured"
)

// Why an apply was not measured.
const (
	NotMeasuredRolledBack   = "rolled back before the measurement window ended"
	NotMeasuredChangedAgain = "the target was changed again by Evolution before the measurement window ended"
)

// Metric keys. Every compared metric is lower-is-better.
const (
	// Agent: over the agent's runs queued in the window that finished
	// (completed or failed); the sample is how many there were.
	MetricFailureRate     = "failure_rate"
	MetricRetryRate       = "retry_rate"
	MetricAvgDurationSecs = "avg_duration_seconds"

	// Workflow: over the status changes in the window of issues whose project
	// uses the workflow; the sample is how many issues moved.
	MetricReturnsPerIssue = "returns_per_issue"
	MetricAvgDwellHours   = "avg_step_dwell_hours"
	// metricStepDwellPrefix + status key is the average dwell in that step.
	// Recorded for people to read; not compared.
	metricStepDwellPrefix = "step_dwell_hours."
)

// metricSpec is a compared metric. A change counts only when it passes the
// relative threshold and is at least MinDiff in absolute terms, so a rate
// moving from 1% to 2% is not a regression.
type metricSpec struct {
	Key     string
	MinDiff float64
}

var effectMetrics = map[string][]metricSpec{
	TargetAgent: {
		{MetricFailureRate, 0.05},
		{MetricRetryRate, 0.05},
		{MetricAvgDurationSecs, 60},
	},
	TargetWorkflow: {
		{MetricReturnsPerIssue, 0.1},
		{MetricAvgDwellHours, 1},
	},
}

// Metrics are one window's measurements. A metric that cannot be measured
// (no completed run to time, no step that defines a way back) is absent.
type Metrics map[string]float64

// Directions of a Finding.
const (
	DirectionBetter = "better"
	DirectionWorse  = "worse"
	DirectionSame   = "same"
)

// Finding is one compared metric.
type Finding struct {
	Metric    string  `json:"metric"`
	Before    float64 `json:"before"`
	After     float64 `json:"after"`
	Direction string  `json:"direction"`
}

// CompareMetrics compares the metrics a target type is judged on. A metric
// missing from either window is skipped.
func CompareMetrics(targetType string, before, after Metrics, thresholdPercent int) []Finding {
	t := float64(thresholdPercent) / 100
	out := []Finding{}
	for _, spec := range effectMetrics[targetType] {
		b, okB := before[spec.Key]
		a, okA := after[spec.Key]
		if !okB || !okA {
			continue
		}
		f := Finding{Metric: spec.Key, Before: b, After: a, Direction: DirectionSame}
		switch {
		case a-b >= spec.MinDiff && a > b*(1+t):
			f.Direction = DirectionWorse
		case b-a >= spec.MinDiff && a < b*(1-t):
			f.Direction = DirectionBetter
		}
		out = append(out, f)
	}
	return out
}

// Verdict draws the verdict: no conclusion below the sample minimum in
// either window; regressed when any metric got worse; improved when one got
// better and none worse.
func Verdict(sampleBefore, sampleAfter, minSample int, findings []Finding) string {
	if sampleBefore < minSample || sampleAfter < minSample {
		return EffectInsufficient
	}
	verdict := EffectUnchanged
	for _, f := range findings {
		switch f.Direction {
		case DirectionWorse:
			return EffectRegressed
		case DirectionBetter:
			verdict = EffectImproved
		}
	}
	return verdict
}

// AgentTaskCounts are an agent's finished runs in one window.
type AgentTaskCounts struct {
	Finished int
	Failed   int
	Retried  int
	// AvgDurationSecs is over completed runs with both timestamps; Timed is
	// how many there were.
	AvgDurationSecs float64
	Timed           int
}

// AgentMetrics turns a window's counts into metrics.
func AgentMetrics(c AgentTaskCounts) (Metrics, int) {
	m := Metrics{}
	if c.Finished > 0 {
		m[MetricFailureRate] = round4(float64(c.Failed) / float64(c.Finished))
		m[MetricRetryRate] = round4(float64(c.Retried) / float64(c.Finished))
	}
	if c.Timed > 0 {
		m[MetricAvgDurationSecs] = round4(c.AvgDurationSecs)
	}
	return m, c.Finished
}

// StatusChange is one status change of an issue in a window, with how long
// the issue had been in From.
type StatusChange struct {
	IssueID string
	From    string
	To      string
	// FromCategory is From's status category; dwell is not counted for the
	// backlog and the closed categories.
	FromCategory string
	DwellSecs    float64
}

// ReturnsOf is the "returned" relation of a workflow: step status key → the
// status its "needs changes" command sends the issue back to
// (back_status_key). It is read from the steps as they were in the window
// measured, so a proposal that changes the way back is judged by each
// window's own definition. A workflow without any way back has none, and
// returns are not measured for it.
func ReturnsOf(stepsJSON string) map[string]string {
	steps, err := issueworkflow.DecodeSteps([]byte(stepsJSON))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, s := range steps {
		if s.BackStatusKey != "" && s.BackStatusKey != s.StatusKey {
			out[s.StatusKey] = s.BackStatusKey
		}
	}
	return out
}

// dwellExcluded are the categories an issue rests in rather than works
// through.
var dwellExcluded = map[string]bool{"backlog": true, "done": true, "cancelled": true}

// WorkflowMetrics turns a window's status changes into metrics.
//
// A return is a status change from a step to exactly that step's
// back_status_key: what the workflow's "needs changes" command does. Moving
// back by hand to the same status counts too; moving anywhere else does not.
// returns_per_issue is returns over the issues that moved in the window.
func WorkflowMetrics(changes []StatusChange, returns map[string]string) (Metrics, int) {
	m := Metrics{}
	issues := map[string]bool{}
	returned := 0
	dwell := map[string][]float64{}
	var total float64
	var counted int
	for _, c := range changes {
		issues[c.IssueID] = true
		if back, ok := returns[c.From]; ok && back == c.To {
			returned++
		}
		if dwellExcluded[c.FromCategory] || c.DwellSecs < 0 {
			continue
		}
		hours := c.DwellSecs / 3600
		dwell[c.From] = append(dwell[c.From], hours)
		total += hours
		counted++
	}
	if len(issues) > 0 && len(returns) > 0 {
		m[MetricReturnsPerIssue] = round4(float64(returned) / float64(len(issues)))
	}
	if counted > 0 {
		m[MetricAvgDwellHours] = round4(total / float64(counted))
	}
	for key, hs := range dwell {
		var sum float64
		for _, h := range hs {
			sum += h
		}
		m[metricStepDwellPrefix+key] = round4(sum / float64(len(hs)))
	}
	return m, len(issues)
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// EffectNotice is the regression comment the Evolution agent leaves on the
// proposal issue, and the owners' inbox item.
type EffectNotice struct {
	// AgentName is the Evolution agent's name; it picks the language.
	AgentName                 string
	TargetType                string
	TargetName                string
	Field                     string
	Version                   int
	WindowDays                int
	SampleBefore, SampleAfter int
	ThresholdPercent          int
	Findings                  []Finding
}

type effectCopy struct {
	title, intro, table, samples, close string
	sampleUnit                          map[string]string
	targets                             map[string]string
	metrics                             map[string]string
}

var effectCopies = map[string]effectCopy{
	"en": {
		title:      "Evolution suggests rolling back: %s %s got worse",
		intro:      "**Suggest rolling back.** In the %[3]d days after version %[4]d of `%[5]s` was applied, the %[1]s **%[2]s** did worse than in the %[3]d days before (by more than %[6]d%%):",
		table:      "| Metric | Before | After |\n|---|---|---|\n",
		samples:    "Sample: %d %s before, %d after.",
		close:      "Nothing was rolled back. Other changes in the same period can move these numbers too; if the change is still to blame, **Roll back** on this issue undoes it.",
		sampleUnit: map[string]string{TargetAgent: "runs", TargetWorkflow: "issues"},
		targets:    map[string]string{TargetAgent: "agent", TargetWorkflow: "workflow"},
		metrics: map[string]string{
			MetricFailureRate: "Failed runs", MetricRetryRate: "Retried runs", MetricAvgDurationSecs: "Average run time",
			MetricReturnsPerIssue: "Returns per issue", MetricAvgDwellHours: "Average time in a step",
		},
	},
	"zh": {
		title:      "进化建议回滚：%s %s 变差了",
		intro:      "**建议回滚。** `%[5]s` 第 %[4]d 版应用后的 %[3]d 天里，%[1]s **%[2]s** 比应用前 %[3]d 天变差（超过 %[6]d%%）：",
		table:      "| 指标 | 应用前 | 应用后 |\n|---|---|---|\n",
		samples:    "样本：应用前 %d %s，应用后 %d。",
		close:      "没有自动回滚。同期的其他改动也可能影响这些指标；确认是这次改动造成的，可在本 issue 上点「回滚」撤销。",
		sampleUnit: map[string]string{TargetAgent: "次运行", TargetWorkflow: "个 issue"},
		targets:    map[string]string{TargetAgent: "智能体", TargetWorkflow: "工作流"},
		metrics: map[string]string{
			MetricFailureRate: "运行失败率", MetricRetryRate: "重试率", MetricAvgDurationSecs: "平均运行时长",
			MetricReturnsPerIssue: "每个 issue 被退回次数", MetricAvgDwellHours: "各步平均停留时长",
		},
	},
}

// Title is the inbox item's title.
func (n EffectNotice) Title() string {
	c := effectCopies[proposalLanguage(n.AgentName)]
	return fmt.Sprintf(c.title, c.targets[n.TargetType], n.TargetName)
}

// Body is the comment, and the inbox item's body. It lists only the metrics
// that got worse.
func (n EffectNotice) Body() string {
	c := effectCopies[proposalLanguage(n.AgentName)]
	var b strings.Builder
	fmt.Fprintf(&b, c.intro, c.targets[n.TargetType], n.TargetName, n.WindowDays, n.Version, n.Field, n.ThresholdPercent)
	b.WriteString("\n\n")
	b.WriteString(c.table)
	worse := []Finding{}
	for _, f := range n.Findings {
		if f.Direction == DirectionWorse {
			worse = append(worse, f)
		}
	}
	sort.SliceStable(worse, func(i, j int) bool { return worse[i].Metric < worse[j].Metric })
	for _, f := range worse {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.metrics[f.Metric], formatMetric(f.Metric, f.Before), formatMetric(f.Metric, f.After))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, c.samples, n.SampleBefore, c.sampleUnit[n.TargetType], n.SampleAfter)
	b.WriteString("\n\n")
	b.WriteString(c.close)
	return b.String()
}

func formatMetric(key string, v float64) string {
	switch key {
	case MetricFailureRate, MetricRetryRate:
		return fmt.Sprintf("%.1f%%", v*100)
	case MetricAvgDurationSecs:
		return fmt.Sprintf("%.0fs", v)
	case MetricAvgDwellHours:
		return fmt.Sprintf("%.1fh", v)
	}
	return fmt.Sprintf("%.2f", v)
}
