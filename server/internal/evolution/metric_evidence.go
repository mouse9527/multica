// Added by the mouse9527/multica fork: Evolution structural analysis, metric
// evidence (M-296).

package evolution

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// EvidenceMetric is a number from the metrics source, cited by reference.
// The server recomputes it over the structural review's window and checks
// both the cited value and its sample.
const EvidenceMetric = "metric"

// MetricDefaultWorkflow names the built-in Default workflow (one the
// workspace never stored) in a metric reference.
const MetricDefaultWorkflow = "default"

// Metric reference forms:
//
//	workflow:<workflow id|default>/issues
//	workflow:<workflow id|default>/step:<status key>/<step metric>
//	agent:<agent id>/<agent metric>
//	issue:<issue id>/returns
//
// The names are the metrics source's JSON keys.
var (
	stepMetricNames  = []string{"entries", "exits", "returns", "return_rate", "dwell_p50_hours", "dwell_p90_hours"}
	agentMetricNames = []string{"runs", "finished", "completed", "failed", "cancelled", "failure_rate", "cancel_rate",
		"duration_p50_seconds", "short_runs", "short_run_rate", "handoffs_in", "handoffs_out"}
)

// MetricRef is a parsed metric reference.
type MetricRef struct {
	Kind string // "workflow", "agent", or "issue"
	ID   string // lower-cased uuid, or MetricDefaultWorkflow
	Step string // workflow step metrics only
	Name string
}

func (r MetricRef) String() string {
	if r.Kind == "workflow" && r.Step != "" {
		return fmt.Sprintf("workflow:%s/step:%s/%s", r.ID, r.Step, r.Name)
	}
	return fmt.Sprintf("%s:%s/%s", r.Kind, r.ID, r.Name)
}

// subject is what makes two metric items independent: the same workflow
// step, agent, or issue is one source however many of its numbers are cited.
func (r MetricRef) subject() string {
	if r.Kind == "workflow" && r.Step != "" {
		return "metric:workflow:" + r.ID + "/step:" + r.Step
	}
	return "metric:" + r.Kind + ":" + r.ID
}

func metricNameIn(name string, names []string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// ParseMetricRef parses a metric reference; the error is client-facing.
func ParseMetricRef(s string) (MetricRef, error) {
	kind, rest, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return MetricRef{}, fmt.Errorf("a metric id looks like agent:<agent id>/failure_rate, workflow:<workflow id>/step:<status key>/return_rate, workflow:<workflow id>/issues, or issue:<issue id>/returns")
	}
	parts := strings.Split(rest, "/")
	ref := MetricRef{Kind: kind, ID: strings.ToLower(parts[0])}
	validID := func() error {
		if ref.Kind == "workflow" && ref.ID == MetricDefaultWorkflow {
			return nil
		}
		if _, err := util.ParseUUID(ref.ID); err != nil {
			return fmt.Errorf("%q is not a uuid", parts[0])
		}
		return nil
	}
	switch {
	case kind == "agent" && len(parts) == 2:
		ref.Name = parts[1]
		if !metricNameIn(ref.Name, agentMetricNames) {
			return MetricRef{}, fmt.Errorf("an agent metric is one of %s", strings.Join(agentMetricNames, ", "))
		}
	case kind == "issue" && len(parts) == 2:
		ref.Name = parts[1]
		if ref.Name != "returns" {
			return MetricRef{}, fmt.Errorf("an issue metric is returns")
		}
	case kind == "workflow" && len(parts) == 2:
		ref.Name = parts[1]
		if ref.Name != "issues" {
			return MetricRef{}, fmt.Errorf("a workflow metric is issues, or a step's: workflow:<id>/step:<status key>/<metric>")
		}
	case kind == "workflow" && len(parts) == 3 && strings.HasPrefix(parts[1], "step:"):
		ref.Step, ref.Name = strings.TrimPrefix(parts[1], "step:"), parts[2]
		if ref.Step == "" {
			return MetricRef{}, fmt.Errorf("the step's status key is missing")
		}
		if !metricNameIn(ref.Name, stepMetricNames) {
			return MetricRef{}, fmt.Errorf("a step metric is one of %s", strings.Join(stepMetricNames, ", "))
		}
	default:
		return MetricRef{}, fmt.Errorf("a metric id looks like agent:<agent id>/failure_rate, workflow:<workflow id>/step:<status key>/return_rate, workflow:<workflow id>/issues, or issue:<issue id>/returns")
	}
	if err := validID(); err != nil {
		return MetricRef{}, err
	}
	return ref, nil
}

// MetricReading is a cited metric as the server recomputed it.
type MetricReading struct {
	Value       float64
	Sample      int
	WindowFrom  time.Time
	WindowUntil time.Time
}

// LookupMetric reads ref out of a summary: the value and the sample it was
// computed over. ok is false, with the reason, when the summary does not
// have it (nothing in the window, or not measurable).
func LookupMetric(s MetricsSummary, ref MetricRef) (value float64, sample int, reason string, ok bool) {
	optional := func(v *float64, n int) (float64, int, string, bool) {
		if v == nil {
			return 0, 0, fmt.Sprintf("%s was not measured in the window", ref), false
		}
		return *v, n, "", true
	}
	count := func(v, n int) (float64, int, string, bool) { return float64(v), n, "", true }
	switch ref.Kind {
	case "agent":
		for _, a := range s.Agents {
			if a.ID != ref.ID {
				continue
			}
			switch ref.Name {
			case "runs":
				return count(a.Runs, a.Runs)
			case "finished":
				return count(a.Finished, a.Finished)
			case "completed":
				return count(a.Completed, a.Finished)
			case "failed":
				return count(a.Failed, a.Finished)
			case "cancelled":
				return count(a.Cancelled, a.Finished)
			case "failure_rate":
				return optional(a.FailureRate, a.Finished)
			case "cancel_rate":
				return optional(a.CancelRate, a.Finished)
			case "duration_p50_seconds":
				return optional(a.DurationP50Secs, a.DurationSample)
			case "short_runs":
				return count(a.ShortRuns, a.DurationSample)
			case "short_run_rate":
				return optional(a.ShortRunRate, a.DurationSample)
			case "handoffs_in":
				return count(a.HandoffsIn, a.HandoffsIn+a.HandoffsOut)
			case "handoffs_out":
				return count(a.HandoffsOut, a.HandoffsIn+a.HandoffsOut)
			}
		}
		return 0, 0, fmt.Sprintf("agent %s is not in the metrics summary (not visible to every member, excluded, or unknown)", ref.ID), false
	case "issue":
		for _, l := range s.ReturnLoops {
			if l.IssueID == ref.ID {
				return count(l.Returns, l.Returns)
			}
		}
		return 0, 0, fmt.Sprintf("issue %s is not among the window's return loops (returned %d or more times)", ref.ID, MetricsReturnLoopMin), false
	case "workflow":
		for _, wf := range s.Workflows {
			id := wf.ID
			if id == "" {
				id = MetricDefaultWorkflow
			}
			if id != ref.ID {
				continue
			}
			if ref.Step == "" {
				return count(wf.Issues, wf.Issues)
			}
			for _, st := range wf.Steps {
				if st.StatusKey != ref.Step {
					continue
				}
				switch ref.Name {
				case "entries":
					return count(st.Entries, st.Entries)
				case "exits":
					return count(st.Exits, st.Exits)
				case "returns":
					if st.BackStatusKey == "" {
						return optional(nil, 0)
					}
					return count(st.Returns, st.Exits)
				case "return_rate":
					return optional(st.ReturnRate, st.Exits)
				case "dwell_p50_hours":
					return optional(st.DwellP50Hours, st.DwellSample)
				case "dwell_p90_hours":
					return optional(st.DwellP90Hours, st.DwellSample)
				}
			}
			return 0, 0, fmt.Sprintf("status %s had no change in workflow %s in the window", ref.Step, ref.ID), false
		}
		return 0, 0, fmt.Sprintf("workflow %s had no status change in the window", ref.ID), false
	}
	return 0, 0, fmt.Sprintf("%s is not a metric", ref), false
}

// metricMatches reports whether a cited value agrees with the recomputed
// one: within 0.01, or 1% of it, whichever is larger, so rounding is fine.
func metricMatches(cited, actual float64) bool {
	return math.Abs(cited-actual) <= math.Max(0.01, 0.01*math.Abs(actual))
}

// CheckMetric checks one cited metric against the summary: it exists, its
// sample reaches minSample, and the cited value is the recomputed one. The
// error is a *RejectedError.
func CheckMetric(i int, s MetricsSummary, ref MetricRef, cited float64, minSample int) (MetricReading, error) {
	value, sample, reason, ok := LookupMetric(s, ref)
	if !ok {
		return MetricReading{}, reject(RejectEvidence, "evidence[%d]: %s", i, reason)
	}
	if sample < minSample {
		return MetricReading{}, reject(RejectEvidence, "evidence[%d]: %s rests on %d, fewer than the %d a metric needs (min_effect_sample)", i, ref, sample, minSample)
	}
	if !metricMatches(cited, value) {
		return MetricReading{}, reject(RejectEvidence, "evidence[%d]: %s is %s over the structural review's window, not %s; cite the number the metrics source gives",
			i, ref, formatMetricValue(value), formatMetricValue(cited))
	}
	return MetricReading{Value: value, Sample: sample, WindowFrom: s.WindowFrom, WindowUntil: s.WindowUntil}, nil
}

func formatMetricValue(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.4g", v)
}
