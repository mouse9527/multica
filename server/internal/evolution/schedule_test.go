// Added by the mouse9527/multica fork: Evolution P2 (M-136).

package evolution

import (
	"testing"
	"time"
)

func shanghai(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func date(t *testing.T, s string) *time.Time {
	t.Helper()
	v, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return &v
}

func enabledConfig() Config {
	cfg := DefaultConfig()
	cfg.Enabled = true
	return cfg
}

func TestPlanRunScheduledSlots(t *testing.T) {
	cursors := func(ts time.Time) map[string]Position { return map[string]Position{SourceComment: {TS: ts}} }
	cases := []struct {
		name     string
		now      string
		lastSlot *time.Time
		started  string
		wantDue  bool
		wantSlot string
	}{
		{name: "first slot after enabling", now: "2026-11-20 03:05", started: "2026-11-19 10:00", wantDue: true, wantSlot: "2026-11-20"},
		{name: "enabled after today's slot waits for tomorrow", now: "2026-11-19 10:05", started: "2026-11-19 10:00"},
		{name: "before today's slot, yesterday's was taken", now: "2026-11-20 02:00", lastSlot: date(t, "2026-11-19"), started: "2026-11-01 00:00"},
		{name: "today's slot is due", now: "2026-11-20 03:00", lastSlot: date(t, "2026-11-19"), started: "2026-11-01 00:00", wantDue: true, wantSlot: "2026-11-20"},
		{name: "today's slot already ran", now: "2026-11-20 23:00", lastSlot: date(t, "2026-11-20"), started: "2026-11-01 00:00"},
		{name: "missed days run once, as soon as the server is back", now: "2026-11-23 01:00", lastSlot: date(t, "2026-11-19"), started: "2026-11-01 00:00", wantDue: true, wantSlot: "2026-11-22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, ok := planRun(scheduleInput{
				Now:      shanghai(t, tc.now),
				Config:   enabledConfig(),
				LastSlot: tc.lastSlot,
				Cursors:  cursors(shanghai(t, tc.started)),
			})
			if ok != tc.wantDue {
				t.Fatalf("due = %v, want %v (plan %+v)", ok, tc.wantDue, plan)
			}
			if !ok {
				return
			}
			if plan.Kind != RunScheduled || plan.Slot == nil || plan.Slot.Format(time.DateOnly) != tc.wantSlot {
				t.Fatalf("plan = %+v, want scheduled slot %s", plan, tc.wantSlot)
			}
		})
	}
}

func TestPlanRunIntervalDays(t *testing.T) {
	cfg := enabledConfig()
	cfg.Schedule.IntervalDays = 3
	in := scheduleInput{
		Config:   cfg,
		LastSlot: date(t, "2026-11-19"),
		Cursors:  map[string]Position{SourceComment: {TS: shanghai(t, "2026-11-01 00:00")}},
	}
	in.Now = shanghai(t, "2026-11-21 04:00")
	if _, ok := planRun(in); ok {
		t.Fatal("two days after the last slot must not be due with interval_days=3")
	}
	in.Now = shanghai(t, "2026-11-22 03:00")
	if plan, ok := planRun(in); !ok || plan.Slot.Format(time.DateOnly) != "2026-11-22" {
		t.Fatalf("plan = %+v, %v; want the 2026-11-22 slot", plan, ok)
	}
}

func TestPlanRunCatchUp(t *testing.T) {
	until := shanghai(t, "2026-11-20 02:55")
	base := scheduleInput{
		Now:      shanghai(t, "2026-11-20 03:30"),
		Config:   enabledConfig(),
		LastSlot: date(t, "2026-11-20"),
		LastRun:  &Run{Status: RunSucceeded, WindowUntil: until},
		Cursors:  map[string]Position{SourceComment: {TS: until.Add(-time.Hour), ID: "01a1ed8e-bce0-7505-822b-f98c4ab3a6ca"}},
	}
	if plan, ok := planRun(base); !ok || plan.Kind != RunCatchUp {
		t.Fatalf("a successful run that stopped short of its window must get a catch-up run, got %+v %v", plan, ok)
	}

	done := base
	done.Cursors = map[string]Position{SourceComment: {TS: until}}
	if _, ok := planRun(done); ok {
		t.Fatal("a run that read its whole window needs no catch-up")
	}

	failed := base
	failed.LastRun = &Run{Status: RunFailed, WindowUntil: until}
	if _, ok := planRun(failed); ok {
		t.Fatal("a failed run is retried at the next slot, not right away")
	}

	capped := base
	capped.RunsToday = capped.Config.MaxRunsPerDay
	if _, ok := planRun(capped); ok {
		t.Fatal("catch-up runs must stop at max_runs_per_day")
	}
}

func TestPlanRunNeedsASourceAndTheSwitch(t *testing.T) {
	in := scheduleInput{Now: shanghai(t, "2026-11-20 03:30"), Config: enabledConfig()}
	if _, ok := planRun(in); ok {
		t.Fatal("no source is being read, so no run")
	}
	in.Cursors = map[string]Position{SourceComment: {TS: shanghai(t, "2026-11-01 00:00")}}
	in.Config.Enabled = false
	if _, ok := planRun(in); ok {
		t.Fatal("analysis is switched off, so no run")
	}
}
