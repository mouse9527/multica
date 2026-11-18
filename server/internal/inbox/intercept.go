// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).
//
// This file is the fork's; create.go only calls intercept.

package inbox

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Interceptor is asked about every inbox write before it is made. Returning
// hold=true keeps the item out of the inbox: createIfEligible then reports
// inserted=false. The fork's Router registers one (internal/agentrouter); with
// none registered every write goes through unchanged, as upstream.
//
// The call runs on its own goroutine with a context that ends at the budget.
// An interceptor should honor it: one that keeps running past the budget has
// already lost (the write went ahead) and keeps one of the limited call slots
// until it returns.
type Interceptor interface {
	Intercept(ctx context.Context, params db.CreateInboxItemParams) (hold bool, err error)
}

// InterceptorSlots bounds the interceptor calls running at once, abandoned
// ones included. With every slot taken, writes skip the interceptor. Read at
// SetInterceptor.
var InterceptorSlots = 64

type registration struct {
	interceptor Interceptor
	budget      time.Duration
	slots       chan struct{}
}

var registered atomic.Pointer[registration]

// SetInterceptor registers i, or removes the registration when i is nil.
// budget bounds how long a write waits for one call; past it the write goes
// ahead.
func SetInterceptor(i Interceptor, budget time.Duration) {
	if i == nil {
		registered.Store(nil)
		return
	}
	registered.Store(&registration{interceptor: i, budget: budget, slots: make(chan struct{}, InterceptorSlots)})
}

// InterceptStats counts, since the process started, the writes that went
// ahead without the interceptor's answer: Abandoned waited the whole budget
// (or the writer's context ended first), Saturated found every call slot
// taken and did not call it at all.
type InterceptStats struct {
	Abandoned int64 `json:"abandoned"`
	Saturated int64 `json:"saturated"`
}

var abandoned, saturated atomic.Int64

func CurrentInterceptStats() InterceptStats {
	return InterceptStats{Abandoned: abandoned.Load(), Saturated: saturated.Load()}
}

// intercept reports whether the registered interceptor holds the item. It
// fails open: an error, a panic, no answer within the budget, or no free
// call slot all mean "write it". The writer waits at most the budget whatever
// the interceptor does, since the call runs on its own goroutine; a call
// that ignores its context keeps a slot, so stuck calls cannot pile up past
// InterceptorSlots. With an interceptor registered, an item without an id
// gets one here, so the interceptor and the insert agree on it.
func intercept(ctx context.Context, params *db.CreateInboxItemParams) bool {
	reg := registered.Load()
	if reg == nil {
		return false
	}
	if !params.ID.Valid {
		params.ID = dbid.NewV7()
	}
	select {
	case reg.slots <- struct{}{}:
	default:
		if n := saturated.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("inbox: every interceptor slot is taken; writing the item", "saturated_total", n)
		}
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, reg.budget)
	p := *params
	answer := make(chan bool, 1)
	go func() {
		defer func() { <-reg.slots }()
		defer cancel()
		held := false
		defer func() {
			if r := recover(); r != nil {
				slog.Error("inbox: interceptor panicked; writing the item", "type", p.Type, "panic", r)
				held = false
			}
			answer <- held
		}()
		hold, err := reg.interceptor.Intercept(ctx, p)
		held = err == nil && ctx.Err() == nil && hold
	}()
	select {
	case held := <-answer:
		return held
	case <-ctx.Done():
		if n := abandoned.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("inbox: interceptor did not answer within its budget; writing the item",
				"type", p.Type, "budget", reg.budget, "abandoned_total", n)
		}
		return false
	}
}

// CreateAndAnnounce writes one inbox row through createIfEligible, the
// interceptor included, and announces it on pub the way Deliver does:
// inbox:new, or inbox:snoozed for a row that joined a snooze on its issue.
// It skips Deliver's type registry and preferences. The fork's Router
// notices use it; upstream writes go through Deliver.
func CreateAndAnnounce(ctx context.Context, queries *db.Queries, pub Publisher, params db.CreateInboxItemParams, issueStatus *string) (item db.InboxItem, inserted bool, err error) {
	item, inserted, err = createIfEligible(ctx, queries, params)
	if err != nil || !inserted {
		return item, inserted, err
	}
	publish(pub, newEvent(item, issueStatus))
	return item, true, nil
}
