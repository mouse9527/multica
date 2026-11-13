// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// hangingDB stands in for an exhausted pool or an unreachable database: every
// query blocks until its context ends.
type hangingDB struct{ queries atomic.Int64 }

func (d *hangingDB) Query(ctx context.Context, _ string, _ ...any) (pgx.Rows, error) {
	d.queries.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (d *hangingDB) Exec(ctx context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}

func (d *hangingDB) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	panic("unexpected QueryRow")
}

// TestCacheReloadDoesNotQueuePublishers: while the database hangs, concurrent
// publishers must not line up behind the subscription reload. One publisher
// reloads, within the capture budget; the rest return at once; and the failed
// reload is not retried by the next events until its backoff passes.
func TestCacheReloadDoesNotQueuePublishers(t *testing.T) {
	db := &hangingDB{}
	c := NewCapturer(NewStore(db, nil))
	c.Timeout = 200 * time.Millisecond
	c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	event := events.Event{Type: protocol.EventCommentCreated, WorkspaceID: "ws",
		Payload: map[string]any{"comment": map[string]any{"id": "c1"}}}

	const publishers = 20
	var wg sync.WaitGroup
	start := time.Now()
	for range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Handle(event)
		}()
	}
	wg.Wait()
	// Serialized reloads would take publishers x Timeout (4s here).
	if elapsed := time.Since(start); elapsed > 3*c.Timeout {
		t.Fatalf("%d concurrent publishers took %s with a hanging database; want about one Timeout (%s)",
			publishers, elapsed, c.Timeout)
	}
	if n := db.queries.Load(); n != 1 {
		t.Fatalf("%d reload queries, want 1", n)
	}

	// Within the backoff, further events neither query nor wait.
	start = time.Now()
	for range 5 {
		c.Handle(event)
	}
	if n := db.queries.Load(); n != 1 {
		t.Fatalf("reload retried inside its backoff: %d queries", n)
	}
	if elapsed := time.Since(start); elapsed > c.Timeout {
		t.Fatalf("events during the backoff took %s", elapsed)
	}

	// Once the backoff has passed, the next event reloads again, and a
	// second failure backs off longer.
	c.retryAt = time.Now().Add(-time.Millisecond)
	c.Handle(event)
	if n := db.queries.Load(); n != 2 {
		t.Fatalf("%d reload queries after the backoff, want 2", n)
	}
	if wait := time.Until(c.retryAt); wait <= reloadBackoffBase {
		t.Fatalf("second failure backs off %s, want longer than the first (%s)", wait, reloadBackoffBase)
	}
}
