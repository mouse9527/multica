// Added by the mouse9527/multica fork: Evolution P2, when runs start (M-136).

package evolution

import (
	"strconv"
	"strings"
	"time"
)

// SafetyDelay keeps the newest rows out of a run's window, so a row committed
// late with a slightly earlier created_at is still ahead of the cursor when
// the next run reads.
const SafetyDelay = 5 * time.Minute

// scheduleInput is everything planRun decides from.
type scheduleInput struct {
	Now    time.Time
	Config Config
	// LastSlot is the slot date (midnight UTC) of the latest scheduled run;
	// nil before the first one.
	LastSlot *time.Time
	// Cursors are the sources being read. No cursor, no run.
	Cursors map[string]Position
	// LastRun is the latest run, if any.
	LastRun *Run
	// RunsToday counts runs created since local midnight.
	RunsToday int
}

type runPlan struct {
	Kind string
	// Slot is the scheduled slot's date (midnight UTC); nil for catch-up.
	Slot *time.Time
}

// planRun decides whether a run should start now.
//
// A scheduled run is due once the latest slot time (today's at the
// configured time, or yesterday's before it) is IntervalDays past the last
// scheduled slot. A slot missed while the server was down is therefore taken
// as soon as it is back, and the run reads everything since the cursor. Before
// the first scheduled run, the slot must fall after reading started.
//
// A catch-up run follows a successful run that did not reach the end of its
// window (it hit max_items_per_window or max_window_days), up to
// max_runs_per_day. A failed run is not retried before the next slot; its
// rows stay ahead of the cursor, so nothing is lost.
func planRun(in scheduleInput) (runPlan, bool) {
	if len(in.Cursors) == 0 || !in.Config.Enabled {
		return runPlan{}, false
	}
	loc := in.Config.Location()
	local := in.Now.In(loc)
	hour, minute := parseClock(in.Config.Schedule.Time)
	slotAt := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if local.Before(slotAt) {
		slotAt = slotAt.AddDate(0, 0, -1)
	}
	slotDate := time.Date(slotAt.Year(), slotAt.Month(), slotAt.Day(), 0, 0, 0, 0, time.UTC)

	due := false
	if in.LastSlot == nil {
		started := in.Now
		for _, p := range in.Cursors {
			if p.TS.Before(started) {
				started = p.TS
			}
		}
		due = slotAt.After(started)
	} else {
		last := time.Date(in.LastSlot.Year(), in.LastSlot.Month(), in.LastSlot.Day(), 0, 0, 0, 0, time.UTC)
		due = int(slotDate.Sub(last).Hours()/24) >= in.Config.Schedule.IntervalDays
	}
	if due {
		return runPlan{Kind: RunScheduled, Slot: &slotDate}, true
	}

	if in.LastRun == nil || in.LastRun.Status != RunSucceeded || in.RunsToday >= in.Config.MaxRunsPerDay {
		return runPlan{}, false
	}
	end := Position{TS: in.LastRun.WindowUntil}
	for _, p := range in.Cursors {
		if p.Before(end) {
			return runPlan{Kind: RunCatchUp}, true
		}
	}
	return runPlan{}, false
}

// localMidnight is the start of now's day in the schedule's time zone.
func localMidnight(now time.Time, cfg Config) time.Time {
	local := now.In(cfg.Location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
}

func parseClock(s string) (int, int) {
	h, m, _ := strings.Cut(s, ":")
	hour, _ := strconv.Atoi(h)
	minute, _ := strconv.Atoi(m)
	return hour, minute
}
