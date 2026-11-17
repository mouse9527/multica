// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86), and Router v6 slices 1 (M-96) and 2 (M-97).

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
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// JobNameReconcile is the scheduler's audit key for reconcile. Stable across
// releases.
const JobNameReconcile = "router_reconcile"

// JobNameDispatch is the scheduler's audit key for dispatch.
const JobNameDispatch = "router_dispatch"

// DispatchInterval is how often dispatch looks for due batches. With the
// default 60 s debounce a batch starts 60-75 s after its last event.
const DispatchInterval = 15 * time.Second

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
// listener and the reconcile job, and with router_dispatch also on, the
// dispatch job, which sends chat-bound subscriptions' batches to their chats
// only with router_chat on as well. With router_chat and router_hold_shadow on
// it also registers the shadow hold as the inbox interceptor (hold.go). With
// router_v1 off it registers nothing, so the server behaves exactly as
// upstream. It returns whether it registered capture.
func Register(ctx context.Context, flags *featureflag.Service, bus *events.Bus, db Database, jobs JobRegistrar, tasks *service.TaskService) bool {
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
	if featureflags.RouterDispatchEnabled(ctx, flags) && tasks != nil {
		notices := NewNotifier(store, InboxAdapter{Queries: tasks.Queries, Bus: bus})
		d := NewDispatcher(store, TaskRuns{Tasks: tasks}, notices)
		d.Chat = featureflags.RouterChatEnabled(ctx, flags)
		d.HoldShadow = featureflags.RouterHoldShadowEnabled(ctx, flags)
		if err := jobs.Register(DispatchJob(d)); err != nil {
			slog.Error("router: register dispatch job", "error", err)
		} else {
			slog.Info("router: dispatch enabled", "interval", DispatchInterval, "chat", d.Chat)
			if d.HoldShadow {
				h := NewHoldShadow(store)
				activeHold.Store(h)
				inbox.SetInterceptor(h, DefaultHoldBudget)
				slog.Info("router: hold enabled in shadow mode", "budget", DefaultHoldBudget)
			}
		}
	}
	return true
}

// DispatchJob runs dispatch on the DB-backed scheduler. Overlapping runs are
// safe: every dispatch takes the agent's lock, and settling is conditional.
func DispatchJob(d *Dispatcher) scheduler.JobSpec {
	return scheduler.JobSpec{
		Name:              JobNameDispatch,
		Cadence:           DispatchInterval,
		CatchUpMode:       scheduler.CatchUpLatestOnly,
		CatchUpWindow:     time.Hour,
		RunTimeout:        time.Minute,
		StaleTimeout:      2 * time.Minute,
		HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       1,
		Scopes:            scheduler.StaticScopes(scheduler.ScopeGlobal),
		Handler: func(ctx context.Context, _ scheduler.HandlerInput) (scheduler.HandlerResult, error) {
			res, err := d.Tick(ctx)
			return scheduler.HandlerResult{
				RowsAffected: int64(res.Dispatched),
				Result: map[string]any{
					"dispatched": res.Dispatched, "settled": res.Settled, "released": res.Released,
					"paused": res.Paused, "waiting": res.Waiting, "notices": res.Notices,
				},
			}, err
		},
	}
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
