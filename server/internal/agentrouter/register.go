// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// JobNameReconcile is the scheduler's audit key for reconcile. Stable across
// releases.
const JobNameReconcile = "router_reconcile"

// ReconcileIntervalEnv overrides DefaultReconcileInterval with a Go duration.
const ReconcileIntervalEnv = "MULTICA_ROUTER_RECONCILE_INTERVAL"

const minReconcileInterval = 30 * time.Second

// Database is what the server hands the Router: a pgx pool.
type Database interface {
	Querier
	TxStarter
}

// JobRegistrar is the scheduler manager's registration method.
type JobRegistrar interface {
	Register(spec scheduler.JobSpec) error
}

// Register wires the Router into the server when router_v1 is on: the Bus
// listener and the reconcile job. With the flag off it registers nothing, so
// the server behaves exactly as upstream. It returns whether it registered.
func Register(ctx context.Context, flags *featureflag.Service, bus *events.Bus, db Database, jobs JobRegistrar) bool {
	if !featureflags.RouterV1Enabled(ctx, flags) {
		return false
	}
	interval, err := ReconcileIntervalFromEnv(os.Getenv(ReconcileIntervalEnv))
	if err != nil {
		slog.Warn("router: invalid reconcile interval; using the default",
			"env", ReconcileIntervalEnv, "default", DefaultReconcileInterval, "error", err)
		interval = DefaultReconcileInterval
	}
	store := NewStore(db, db)
	NewCapturer(store).Subscribe(bus)
	if err := jobs.Register(ReconcileJob(NewReconciler(store), interval)); err != nil {
		slog.Error("router: register reconcile job", "error", err)
	}
	slog.Info("router: capture enabled", "reconcile_interval", interval)
	return true
}

// ReconcileIntervalFromEnv parses the reconcile interval override. Empty
// means the default.
func ReconcileIntervalFromEnv(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultReconcileInterval, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < minReconcileInterval {
		return 0, fmt.Errorf("interval %s is below the minimum %s", d, minReconcileInterval)
	}
	return d, nil
}

// ReconcileJob runs reconcile on the DB-backed scheduler, which leases each
// plan to one replica. Reconcile is idempotent, so a stolen stale lease or an
// overlapping run only repeats work.
func ReconcileJob(r *Reconciler, interval time.Duration) scheduler.JobSpec {
	runTimeout := min(interval, 4*time.Minute)
	return scheduler.JobSpec{
		Name:              JobNameReconcile,
		Cadence:           interval,
		CatchUpMode:       scheduler.CatchUpLatestOnly,
		CatchUpWindow:     time.Hour,
		RunTimeout:        runTimeout,
		StaleTimeout:      runTimeout + time.Minute,
		HeartbeatInterval: 15 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       1,
		Scopes:            scheduler.StaticScopes(scheduler.ScopeGlobal),
		Handler: func(ctx context.Context, _ scheduler.HandlerInput) (scheduler.HandlerResult, error) {
			res, err := r.Run(ctx)
			missed := map[string]any{}
			for k, v := range res.Missed {
				missed[k] = v
			}
			return scheduler.HandlerResult{
				RowsAffected: int64(res.Total()),
				Result: map[string]any{
					"workspaces":   res.Workspaces,
					"capture_miss": res.Total(),
					"by_kind":      missed,
				},
			}, err
		},
	}
}
