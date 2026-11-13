// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// pauseFirstBeforeSeenLock stops the first capture or reconcile of issueID
// just before it takes the seen lock, until release is called.
func pauseFirstBeforeSeenLock(t *testing.T, issueID string) (reached <-chan struct{}, release func()) {
	t.Helper()
	reachedC, releaseC := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	testHookBeforeSeenLock = func(_, id string) {
		if id == issueID && first.CompareAndSwap(false, true) {
			close(reachedC)
			<-releaseC
		}
	}
	var once sync.Once
	release = func() { once.Do(func() { close(releaseC) }) }
	t.Cleanup(func() {
		release()
		testHookBeforeSeenLock = nil
	})
	return reachedC, release
}

func (e *env) seenStatus(subID, issueID string) string {
	e.t.Helper()
	var status string
	if err := pool.QueryRow(e.ctx, `SELECT status FROM router_issue_seen WHERE subscription_id = $1 AND issue_id = $2`,
		subID, issueID).Scan(&status); err != nil {
		e.t.Fatalf("seen status: %v", err)
	}
	return status
}

func statusEvents(evs []Event) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Kind == KindStatusChanged {
			out = append(out, ev)
		}
	}
	return out
}

// TestDelayedCaptureNeverRevertsSeen is the interleaving from review: capture
// G1 announces todo->in_progress but stalls before the seen lock; meanwhile
// the issue moves to done and capture G2 records it. When G1 resumes it must
// not compare its stale view with the newer seen row, write a reversed event,
// or move seen back.
func TestDelayedCaptureNeverRevertsSeen(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)

	e.setStatus(root, "in_progress")
	g1 := events.Event{Type: protocol.EventIssueUpdated, WorkspaceID: e.ws, ActorType: "member", ActorID: e.user,
		Payload: map[string]any{"issue": e.issuePayload(root)}}
	reached, release := pauseFirstBeforeSeenLock(t, root)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.capt.Handle(g1)
	}()
	<-reached

	e.setStatus(root, "done")
	e.publishIssue(protocol.EventIssueUpdated, root, "agent", e.agentB)
	release()
	<-done

	evs := statusEvents(e.events(sub.ID))
	if len(evs) != 1 {
		t.Fatalf("status events = %+v, want exactly one", evs)
	}
	ev := evs[0]
	if ev.Payload["from"] != "todo" || ev.Payload["to"] != "done" {
		t.Fatalf("status event %v -> %v, want todo -> done", ev.Payload["from"], ev.Payload["to"])
	}
	if ev.ActorID == nil || *ev.ActorID != e.agentB {
		t.Fatalf("status event actor = %v, want the publisher whose state it recorded (agent B)", ev.ActorID)
	}
	if got := e.seenStatus(sub.ID, root); got != "done" {
		t.Fatalf("seen status = %s, want done", got)
	}
	if res := e.reconcile(); res.Total() != 0 {
		t.Fatalf("reconcile counted %v as missed", res.Missed)
	}
}

// TestDelayedReconcileNeverRevertsSeen: reconcile picks a status difference
// as a candidate and stalls before the seen lock while the listener records
// a newer change. Reconcile must not write the older state back.
func TestDelayedReconcileNeverRevertsSeen(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)

	e.setStatus(root, "in_progress") // never published: a reconcile candidate
	reached, release := pauseFirstBeforeSeenLock(t, root)
	var res ReconcileResult
	var rerr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, rerr = e.recon.RunWorkspace(e.ctx, e.ws)
	}()
	<-reached

	e.setStatus(root, "done")
	e.publishIssue(protocol.EventIssueUpdated, root, "member", e.user)
	release()
	<-done
	if rerr != nil {
		t.Fatalf("reconcile: %v", rerr)
	}

	evs := statusEvents(e.events(sub.ID))
	if len(evs) != 1 || evs[0].Payload["to"] != "done" || evs[0].Source != SourceBus {
		t.Fatalf("status events = %+v, want only the listener's todo -> done", evs)
	}
	if got := e.seenStatus(sub.ID, root); got != "done" {
		t.Fatalf("seen status = %s, want done", got)
	}
	if res.Missed[KindStatusChanged] != 0 {
		t.Fatalf("reconcile counted %d status misses", res.Missed[KindStatusChanged])
	}
}

var errBudget = errors.New("test budget exhausted")

// budgetDB fails every read after its budget, the way a pass that runs out of
// time does. Writes are not limited.
type budgetDB struct {
	Querier
	left atomic.Int64
}

func (b *budgetDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if b.left.Add(-1) < 0 {
		return nil, errBudget
	}
	return b.Querier.Query(ctx, sql, args...)
}

func (b *budgetDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if b.left.Add(-1) < 0 {
		return errRow{}
	}
	return b.Querier.QueryRow(ctx, sql, args...)
}

type errRow struct{}

func (errRow) Scan(...any) error { return errBudget }

// TestReconcileBacklogProgressesUnderSmallBudget: a backlog far larger than
// one pass can read (batches of 2, a handful of reads per pass, ties on one
// timestamp) is worked off over several passes. Every cut-short pass keeps
// its progress, nothing is lost, nothing is added twice, and the workspace is
// only marked reconciled once a pass completes.
func TestReconcileBacklogProgressesUnderSmallBudget(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)

	var comments []string
	for range 7 {
		comments = append(comments, e.comment(root))
	}
	// Four comments on one timestamp: the keyset, not the time alone, has to
	// carry a batch boundary through them.
	e.fx.Exec(t, `UPDATE comment SET created_at = (SELECT max(created_at) FROM comment WHERE id = ANY($1::uuid[]))
		WHERE id = ANY($1::uuid[])`, comments[:4])
	for range 3 {
		e.issue("child", testutil.Cols{"parent_issue_id": root})
		e.failedTask(e.agentB, root)
	}
	const want = 13

	limited := &budgetDB{Querier: pool}
	recon := NewReconciler(NewStore(limited, pool))
	recon.Grace, recon.Batch = 0, 2

	total, prev := 0, 0
	for pass := 1; ; pass++ {
		if pass > 20 {
			t.Fatalf("backlog not worked off after 20 passes: %d of %d events", prev, want)
		}
		// A pass always spends 8 reads before it can read a batch of the last
		// kind (the upper bound, the level candidates, and a cursor read plus
		// one batch per kind). 9 leaves room for one productive batch.
		limited.left.Store(9)
		res, err := recon.RunWorkspace(e.ctx, e.ws)
		total += res.Total()
		got := len(e.events(sub.ID))
		if got >= want {
			break
		}
		if err == nil {
			t.Fatalf("pass %d finished with %d of %d events", pass, got, want)
		}
		if !errors.Is(err, errBudget) {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got <= prev {
			t.Fatalf("pass %d made no progress: %d events", pass, got)
		}
		if e.get(sub.ID).ReconciledThrough != nil {
			t.Fatalf("pass %d was cut short but marked the workspace reconciled", pass)
		}
		prev = got
	}

	limited.left.Store(1 << 30)
	res, err := recon.RunWorkspace(e.ctx, e.ws)
	if err != nil {
		t.Fatalf("final pass: %v", err)
	}
	total += res.Total()
	evs := e.events(sub.ID)
	if len(evs) != want || total != want {
		t.Fatalf("%d events, %d counted as missed; want %d each", len(evs), total, want)
	}
	byKind := map[string]int{}
	for _, ev := range evs {
		byKind[ev.Kind]++
	}
	if byKind[KindCommentCreated] != 7 || byKind[KindIssueCreated] != 3 || byKind[KindTaskFailed] != 3 {
		t.Fatalf("events by kind = %v", byKind)
	}
	got := e.get(sub.ID)
	if got.CaptureMiss != want || got.ReconciledThrough == nil {
		t.Fatalf("capture_miss=%d reconciled_through=%v after the completed pass", got.CaptureMiss, got.ReconciledThrough)
	}
}

// TestCaptureSkipsSubscriptionPausedBehindItsCache: a capture working from a
// cache snapshot taken before a pause writes nothing for the paused
// subscription.
func TestCaptureSkipsSubscriptionPausedBehindItsCache(t *testing.T) {
	e := newEnv(t)
	root := e.issue("root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.capt.TTL = time.Hour
	e.publishComment(e.comment(root)) // loads the cache
	// Paused by another replica: this process's cache does not hear of it.
	e.fx.Exec(t, `UPDATE router_subscription SET enabled = false, paused_reason = 'manual' WHERE id = $1`, sub.ID)
	e.publishComment(e.comment(root))
	if n := len(e.events(sub.ID)); n != 1 {
		t.Fatalf("%d events, want only the one captured before the pause", n)
	}
}
