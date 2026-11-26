// Added by the mouse9527/multica fork: Evolution structural analysis, the
// weekly structural review (M-296).

package evolution

import (
	"strings"
	"testing"
	"time"
)

func TestWeekStartIsMonday(t *testing.T) {
	for day, want := range map[string]string{
		"2026-11-30": "2026-11-30", // Monday
		"2026-12-02": "2026-11-30", // Wednesday
		"2026-12-06": "2026-11-30", // Sunday
		"2026-11-29": "2026-11-23", // Sunday before
		"2027-01-01": "2026-12-28", // across a year
	} {
		if got := weekStart(*date(t, day)).Format(time.DateOnly); got != want {
			t.Errorf("weekStart(%s) = %s, want %s", day, got, want)
		}
	}
}

// TestRunTitleMarksTheStructuralReview: the structural review's title says
// so in the agent's language; other runs keep their titles.
func TestRunTitleMarksTheStructuralReview(t *testing.T) {
	cfg := DefaultConfig()
	now := shanghai(t, "2026-11-30 03:05")
	scheduled := runPlan{Kind: RunScheduled, Slot: date(t, "2026-11-30")}
	if got := runTitle("进化", scheduled, now, cfg, true); got != "进化分析 · 2026-11-30 · 结构复盘" {
		t.Fatalf("zh structural title = %q", got)
	}
	if got := runTitle("Evolution", scheduled, now, cfg, true); got != "Evolution analysis · 2026-11-30 · structural review" {
		t.Fatalf("en structural title = %q", got)
	}
	if got := runTitle("Renamed", scheduled, now, cfg, true); got != "Evolution analysis · 2026-11-30 · structural review" {
		t.Fatalf("renamed agent's structural title = %q", got)
	}
	if got := runTitle("进化", scheduled, now, cfg, false); got != "进化分析 · 2026-11-30" {
		t.Fatalf("zh daily title = %q", got)
	}
}

// TestRunDescriptionListsMetricsOnlyForTheStructuralReview: only the
// structural review's issue lists the metrics source, its 14-day window, and
// how to read it; a daily run's description does not mention metrics.
func TestRunDescriptionListsMetricsOnlyForTheStructuralReview(t *testing.T) {
	until := time.Date(2026, 11, 29, 18, 55, 0, 0, time.UTC)
	run := Run{Kind: RunScheduled, WindowUntil: until}
	cursors := map[string]Position{SourceComment: {TS: until.Add(-24 * time.Hour)}}

	daily := runDescription(run, cursors, false)
	if strings.Contains(daily, "metrics") || strings.Contains(daily, "structural") {
		t.Fatalf("daily description mentions metrics:\n%s", daily)
	}
	review := runDescription(run, cursors, true)
	for _, want := range []string{
		"structural review",
		"- `metrics`: the 14 days up to 2026-11-29T18:55:00Z",
		"multica evolution signals --source metrics --days 14 --output json",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("structural description lacks %q:\n%s", want, review)
		}
	}
	// Everything a daily run says is still there.
	for _, line := range strings.Split(daily, "\n") {
		if strings.HasPrefix(line, "- `") && !strings.Contains(review, line) {
			t.Fatalf("structural description lost %q:\n%s", line, review)
		}
	}
}
