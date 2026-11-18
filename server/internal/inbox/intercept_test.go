// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97).

package inbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// errWritten is what the fake database answers every insert with, so a test
// sees whether createIfEligible went on to write.
var errWritten = errors.New("insert reached the database")

type fakeDB struct{ inserts int }

func (f *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errWritten }
func (f *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row {
	f.inserts++
	return errRow{}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errWritten }

type interceptorFunc func(ctx context.Context, p db.CreateInboxItemParams) (bool, error)

func (f interceptorFunc) Intercept(ctx context.Context, p db.CreateInboxItemParams) (bool, error) {
	return f(ctx, p)
}

func create(t *testing.T, p db.CreateInboxItemParams) (inserted bool, wrote bool) {
	t.Helper()
	fake := &fakeDB{}
	_, inserted, err := createIfEligible(context.Background(), db.New(fake), p)
	if err != nil && !errors.Is(err, errWritten) {
		t.Fatalf("createIfEligible: %v", err)
	}
	return inserted, fake.inserts == 1
}

func register(t *testing.T, f interceptorFunc, budget time.Duration) {
	t.Helper()
	SetInterceptor(f, budget)
	t.Cleanup(func() { SetInterceptor(nil, 0) })
}

func TestNoInterceptorWritesUnchanged(t *testing.T) {
	SetInterceptor(nil, 0)
	if _, wrote := create(t, db.CreateInboxItemParams{Type: "mentioned"}); !wrote {
		t.Fatal("the write did not reach the database")
	}
}

func TestInterceptorFailsOpen(t *testing.T) {
	for name, f := range map[string]interceptorFunc{
		"error": func(context.Context, db.CreateInboxItemParams) (bool, error) {
			return false, errors.New("boom")
		},
		"hold with an error": func(context.Context, db.CreateInboxItemParams) (bool, error) {
			return true, errors.New("boom")
		},
		"panic": func(context.Context, db.CreateInboxItemParams) (bool, error) {
			panic("boom")
		},
		"past the budget": func(ctx context.Context, _ db.CreateInboxItemParams) (bool, error) {
			<-ctx.Done()
			return true, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			register(t, f, 20*time.Millisecond)
			start := time.Now()
			if _, wrote := create(t, db.CreateInboxItemParams{Type: "mentioned"}); !wrote {
				t.Fatal("a failed interceptor dropped the write")
			}
			if took := time.Since(start); took > time.Second {
				t.Fatalf("the write waited %s", took)
			}
		})
	}
}

// blockedInterceptor ignores its context: it returns only when release is
// closed, then answers hold=true.
func blockedInterceptor(release <-chan struct{}, calls *atomic.Int64) interceptorFunc {
	return func(context.Context, db.CreateInboxItemParams) (bool, error) {
		calls.Add(1)
		<-release
		return true, nil
	}
}

func TestInterceptorIgnoringCancellationCostsAtMostTheBudget(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	register(t, blockedInterceptor(release, &calls), 30*time.Millisecond)
	t.Cleanup(func() { close(release) })
	before := CurrentInterceptStats().Abandoned

	start := time.Now()
	inserted, wrote := create(t, db.CreateInboxItemParams{Type: "mentioned"})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the write waited %s for an interceptor that ignores its context", took)
	}
	if !wrote || inserted {
		// The fake database answers every insert with errWritten.
		t.Fatalf("wrote=%v inserted=%v, want the write to reach the database", wrote, inserted)
	}
	if got := CurrentInterceptStats().Abandoned - before; got != 1 {
		t.Fatalf("abandoned = %d, want 1", got)
	}
}

func TestInterceptorAnsweringLateDoesNotHold(t *testing.T) {
	register(t, func(context.Context, db.CreateInboxItemParams) (bool, error) {
		time.Sleep(100 * time.Millisecond) // ignores the context
		return true, nil
	}, 20*time.Millisecond)
	if _, wrote := create(t, db.CreateInboxItemParams{Type: "mentioned"}); !wrote {
		t.Fatal("a hold answered after the budget kept the item out")
	}
}

func TestStuckInterceptorCallsAreBounded(t *testing.T) {
	orig := InterceptorSlots
	InterceptorSlots = 2
	t.Cleanup(func() { InterceptorSlots = orig })
	release := make(chan struct{})
	var calls atomic.Int64
	register(t, blockedInterceptor(release, &calls), 200*time.Millisecond)
	before := CurrentInterceptStats().Saturated

	// Two stuck calls take both slots; the next writes skip the interceptor
	// without waiting at all.
	for range 2 {
		create(t, db.CreateInboxItemParams{Type: "mentioned"})
	}
	for range 3 {
		start := time.Now()
		if _, wrote := create(t, db.CreateInboxItemParams{Type: "mentioned"}); !wrote {
			t.Fatal("a saturated interceptor dropped the write")
		}
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Fatalf("a saturated write waited %s", took)
		}
	}
	if calls.Load() != 2 || CurrentInterceptStats().Saturated-before != 3 {
		t.Fatalf("calls = %d, saturated = %d; want 2 and 3", calls.Load(), CurrentInterceptStats().Saturated-before)
	}

	// Once the stuck calls return, their slots are free again.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		create(t, db.CreateInboxItemParams{Type: "mentioned"})
	}
	if calls.Load() < 3 {
		t.Fatal("the interceptor was never called again after the stuck calls returned")
	}
}

func TestInterceptorHoldKeepsTheItemOut(t *testing.T) {
	register(t, func(context.Context, db.CreateInboxItemParams) (bool, error) { return true, nil }, time.Second)
	inserted, wrote := create(t, db.CreateInboxItemParams{Type: "mentioned"})
	if inserted || wrote {
		t.Fatalf("held item: inserted=%v wrote=%v, want neither", inserted, wrote)
	}
}

func TestInterceptorSeesTheItemID(t *testing.T) {
	var seen db.CreateInboxItemParams
	register(t, func(_ context.Context, p db.CreateInboxItemParams) (bool, error) {
		seen = p
		return false, nil
	}, time.Second)

	// An item without an id gets one before the interceptor sees it.
	create(t, db.CreateInboxItemParams{Type: "mentioned"})
	if !seen.ID.Valid {
		t.Fatal("the interceptor saw no item id")
	}
	// An item with an id keeps it.
	id := dbid.NewV7()
	create(t, db.CreateInboxItemParams{ID: id, Type: "mentioned"})
	if seen.ID != id {
		t.Fatalf("the interceptor saw id %v, want %v", seen.ID, id)
	}
}
