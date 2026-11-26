// Added by the mouse9527/multica fork: Evolution structural analysis, the
// weekly structural review (M-296).

package evolution

import (
	"context"
	"fmt"
	"time"
)

// The weekly structural review is not a run kind of its own (decision of
// 2026-11-26: no new kind, no schema change). While the metrics source is on,
// the week's first scheduled run that did not fail is the structural review:
// its analysis issue lists the metrics source with a StructureReviewDays
// window and its title says so. That is the Monday run, or, when there is
// none on Monday, the next scheduled run of the week. Weeks run Monday to
// Sunday in the schedule's time zone; a run's slot is already that local
// date.

// StructureReviewDays is the metrics window a structural review reads.
const StructureReviewDays = DefaultMetricsWindowDays

// ErrNotStructureRun: the metrics source is on, but the open run is not the
// week's structural review. It is ErrSourceOff to clients.
var ErrNotStructureRun = fmt.Errorf("%w: only the week's structural review run reads metrics", ErrSourceOff)

// weekStart is the Monday of the week holding day (a local date at midnight
// UTC, as a run's slot is).
func weekStart(day time.Time) time.Time {
	back := (int(day.Weekday()) + 6) % 7
	return time.Date(day.Year(), day.Month(), day.Day()-back, 0, 0, 0, 0, time.UTC)
}

// structureSlot reports whether runID, a scheduled run on slot, is its
// week's structural review: no other scheduled run earlier that week, other
// than one that failed.
func structureSlot(ctx context.Context, q Querier, workspaceID string, slot time.Time, runID string) (bool, error) {
	var earlier bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM evolution_run
			WHERE workspace_id = $1::uuid AND kind = 'scheduled' AND status <> 'failed'
			  AND slot >= $2::date AND slot < $3::date AND id <> $4::uuid)`,
		workspaceID, weekStart(slot).Format(time.DateOnly), slot.Format(time.DateOnly), runID).Scan(&earlier)
	return !earlier, err
}

// isStructureRun reports whether run is its week's structural review. A
// catch-up run never is.
func isStructureRun(ctx context.Context, q Querier, run Run) (bool, error) {
	if run.Kind != RunScheduled || run.Slot == nil {
		return false, nil
	}
	return structureSlot(ctx, q, run.WorkspaceID, *run.Slot, run.ID)
}

// Structural review titles follow the agent's enable-time language, like the
// analysis title.
var structureTitleSuffix = map[string]string{
	"Evolution": "structural review",
	"进化":        "结构复盘",
	"진화":        "구조 점검",
	"進化":        "構造レビュー",
}

func structureTitle(agentName string) string {
	if s, ok := structureTitleSuffix[agentName]; ok {
		return s
	}
	return structureTitleSuffix["Evolution"]
}
