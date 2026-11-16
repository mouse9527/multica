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
// hold=true keeps the item out of the inbox: CreateIfEligible then reports
// inserted=false. The fork's Router registers one (internal/agentrouter); with
// none registered every write goes through unchanged, as upstream.
type Interceptor interface {
	Intercept(ctx context.Context, params db.CreateInboxItemParams) (hold bool, err error)
}

type registration struct {
	interceptor Interceptor
	budget      time.Duration
}

var registered atomic.Pointer[registration]

// SetInterceptor registers i, or removes the registration when i is nil.
// budget bounds how long one call may take; past it the write goes ahead.
func SetInterceptor(i Interceptor, budget time.Duration) {
	if i == nil {
		registered.Store(nil)
		return
	}
	registered.Store(&registration{interceptor: i, budget: budget})
}

// intercept reports whether the registered interceptor holds the item. It
// fails open: an error, a panic, or running past the budget all mean "write
// it", so a broken interceptor can delay an inbox write by at most the budget
// and never drop one. With an interceptor registered, an item without an id
// gets one here, so the interceptor and the insert agree on it.
func intercept(ctx context.Context, params *db.CreateInboxItemParams) (hold bool) {
	reg := registered.Load()
	if reg == nil {
		return false
	}
	if !params.ID.Valid {
		params.ID = dbid.NewV7()
	}
	ctx, cancel := context.WithTimeout(ctx, reg.budget)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("inbox: interceptor panicked; writing the item", "type", params.Type, "panic", r)
			hold = false
		}
	}()
	held, err := reg.interceptor.Intercept(ctx, *params)
	return err == nil && ctx.Err() == nil && held
}
