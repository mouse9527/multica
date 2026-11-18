// Modified for the mouse9527/multica fork: Router parallelism (M-104).

package agentrouter

import (
	"sort"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// claim claims the agent's next task the way the daemon does, honouring
// max_concurrent_tasks, and returns its id or "".
func (e *env) claim(agentID string) string {
	e.t.Helper()
	svc := service.NewTaskService(db.New(pool), pool, nil, events.New())
	task, err := svc.ClaimTask(e.ctx, util.MustParseUUID(agentID))
	if err != nil {
		e.t.Fatalf("claim: %v", err)
	}
	if task == nil {
		return ""
	}
	return util.UUIDToString(task.ID)
}

func (e *env) taskStatus(taskID string) string {
	e.t.Helper()
	var status string
	if err := pool.QueryRow(e.ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status); err != nil {
		e.t.Fatal(err)
	}
	return status
}

// batchReady gives sub one new event from someone else, old enough to be due.
func (e *env) batchReady(sub Subscription, root string) {
	e.t.Helper()
	e.publishComment(e.comment(root))
	e.age(sub.ID)
}

func TestAnAgentsChatRoutersDispatchAndRunInParallel(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	rootA, rootB := e.issue("router a"), e.issue("router b")
	subA := e.subscribe(e.agentA, ScopeIssueTree, rootA)
	subB := e.subscribe(e.agentA, ScopeIssueTree, rootB)
	chatA, chatB := e.bindChat(subA), e.bindChat(subB)
	e.batchReady(subA, rootA)
	e.batchReady(subB, rootB)

	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d, want both of the agent's Routers dispatched in one tick", len(runs.calls))
	}
	got := []string{runs.calls[0].chat, runs.calls[1].chat}
	want := []string{chatA, chatB}
	sort.Strings(got)
	sort.Strings(want)
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("batches went to %v, want one into each chat %v", got, want)
	}

	// Within the agent's capacity the two turns run at the same time.
	e.fx.Exec(t, `UPDATE agent SET max_concurrent_tasks = 2 WHERE id = $1`, e.agentA)
	first, second := e.claim(e.agentA), e.claim(e.agentA)
	if first == "" || second == "" || first == second {
		t.Fatalf("claimed %q and %q, want both batch turns running", first, second)
	}

	// A new batch for one Router waits for that Router's own turn only.
	e.batchReady(subA, rootA)
	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d, want chat A's next batch to wait for its open turn", len(runs.calls))
	}
	turnA := runs.calls[0].task
	if runs.calls[1].chat == chatA {
		turnA = runs.calls[1].task
	}
	e.setTask(turnA, "completed")
	e.tick(d)
	if len(runs.calls) != 3 || runs.calls[2].chat != chatA {
		t.Fatalf("runs = %+v, want chat A's next batch while chat B's turn is still running", runs.calls)
	}
}

// TestOneChatTakesOneBatchAtATime: subscriptions bound to the same chat share
// its conversation, so the chat takes one batch turn at a time from any of
// them.
func TestOneChatTakesOneBatchAtATime(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root1, root2 := e.issue("router one"), e.issue("router two")
	sub1 := e.subscribe(e.agentA, ScopeIssueTree, root1)
	sub2 := e.subscribe(e.agentA, ScopeIssueTree, root2)
	chat := e.bindChat(sub1)
	if _, err := e.store.Update(e.ctx, e.ws, sub2.ID, UpdateParams{AnchorChatSessionID: &chat, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	e.batchReady(sub1, root1)
	e.batchReady(sub2, root2)

	e.tick(d)
	e.tick(d)
	if len(runs.calls) != 1 || runs.calls[0].chat != chat {
		t.Fatalf("runs = %+v, want one batch turn in the shared chat", runs.calls)
	}
	e.setTask(runs.calls[0].task, "completed")
	e.tick(d)
	if len(runs.calls) != 2 || runs.calls[1].chat != chat {
		t.Fatalf("runs = %+v, want the other subscription's batch once the first turn ended", runs.calls)
	}
	var subs []string
	if err := pool.QueryRow(e.ctx, `SELECT array_agg(subscription_id::text ORDER BY created_at) FROM router_dispatch WHERE task_id = ANY($1::uuid[])`,
		[]string{runs.calls[0].task, runs.calls[1].task}).Scan(&subs); err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0] == subs[1] {
		t.Fatalf("dispatches came from %v, want one from each subscription", subs)
	}

	// The same subscription's next batch waits for the open turn too.
	e.batchReady(sub1, root1)
	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d, want the chat's next batch to wait", len(runs.calls))
	}
}

// TestAnchorRunBeforeBindingHoldsTheChat: an anchor run a subscription
// started before it was bound to a chat holds its first batch into the chat.
func TestAnchorRunBeforeBindingHoldsTheChat(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.batchReady(sub, root)
	e.tick(d)
	if len(runs.calls) != 1 || runs.calls[0].issue != root {
		t.Fatalf("runs = %+v, want an anchor run", runs.calls)
	}
	chat := e.bindChat(sub)
	e.batchReady(sub, root)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %+v, want the chat batch to wait for the open anchor run", runs.calls)
	}
	e.setTask(runs.calls[0].task, "completed")
	e.tick(d)
	if len(runs.calls) != 2 || runs.calls[1].chat != chat {
		t.Fatalf("runs = %+v, want the batch in the chat once the anchor run ended", runs.calls)
	}
}

// TestConcurrentDispatchesShareOneChat: while one dispatch into a chat holds
// its locks, a concurrent tick must not start a second batch turn in that
// chat from another subscription, nor the same subscription again, while an
// unrelated Router of the same agent goes ahead.
func TestConcurrentDispatchesShareOneChat(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root1, root2, root3 := e.issue("router one"), e.issue("router two"), e.issue("router three")
	sub1 := e.subscribe(e.agentA, ScopeIssueTree, root1)
	sub2 := e.subscribe(e.agentA, ScopeIssueTree, root2)
	sub3 := e.subscribe(e.agentA, ScopeIssueTree, root3)
	chat := e.bindChat(sub1)
	if _, err := e.store.Update(e.ctx, e.ws, sub2.ID, UpdateParams{AnchorChatSessionID: &chat, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	e.batchReady(sub1, root1)

	reached, held := make(chan struct{}), make(chan struct{})
	var hold, unhold sync.Once
	release := func() { unhold.Do(func() { close(held) }) }
	testHookBeforeEnqueue = func(id string) {
		if id == sub1.ID {
			hold.Do(func() {
				close(reached)
				<-held
			})
		}
	}
	var heldErr error
	finished := make(chan struct{})
	// Registered last, so it runs first: a failing test lets the held
	// dispatch finish before the fixture cleanup needs its row locks.
	t.Cleanup(func() {
		release()
		<-finished
		testHookBeforeEnqueue = nil
	})

	go func() {
		defer close(finished)
		_, heldErr = d.TickWorkspace(e.ctx, e.ws)
	}()
	select {
	case <-reached:
	case <-finished:
		t.Fatalf("sub1's dispatch never got to the enqueue (tick error: %v)", heldErr)
	}

	// sub1's dispatch holds its locks but has not committed. sub2 (same chat)
	// and sub1 again must wait; sub3 (no chat) is another Router and goes.
	e.batchReady(sub2, root2)
	e.batchReady(sub3, root3)
	e.tick(d)
	runs.mu.Lock()
	during := append([]fakeRun(nil), runs.calls...)
	runs.mu.Unlock()
	if len(during) != 1 || during[0].issue != root3 {
		t.Fatalf("runs during the held dispatch = %+v, want only sub3's anchor run", during)
	}

	release()
	<-finished
	if heldErr != nil {
		t.Fatalf("held tick: %v", heldErr)
	}
	var inChat int
	if err := pool.QueryRow(e.ctx, `
		SELECT count(*) FROM router_dispatch dp JOIN agent_task_queue t ON t.id = dp.task_id
		WHERE dp.settled_at IS NULL AND t.chat_session_id = $1`, chat).Scan(&inChat); err != nil {
		t.Fatal(err)
	}
	if inChat != 1 {
		t.Fatalf("open batch turns in the chat = %d, want 1", inChat)
	}
}

// TestFullCapacityQueuesBatches: with the agent at max_concurrent_tasks, its
// Routers still dispatch; their runs wait in the queue holding their batches
// and are claimed as capacity frees up.
func TestFullCapacityQueuesBatches(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	busy := e.issue("other work")
	running := e.fx.Task(t, e.agentA, testutil.Cols{"issue_id": busy, "runtime_id": e.runtimeOf(e.agentA), "status": "running"})
	rootA, rootB := e.issue("router a"), e.issue("router b")
	subA := e.subscribe(e.agentA, ScopeIssueTree, rootA)
	subB := e.subscribe(e.agentA, ScopeIssueTree, rootB)
	e.batchReady(subA, rootA)
	e.batchReady(subB, rootB)

	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d, want both Routers dispatched while the agent is busy", len(runs.calls))
	}
	for _, sub := range []Subscription{subA, subB} {
		if p := e.pending(sub.ID); len(p) != 0 {
			t.Fatalf("subscription %s pending = %d, want its batch held by its queued run", sub.ID, len(p))
		}
	}
	if got := e.claim(e.agentA); got != "" {
		t.Fatalf("claimed %s with the agent at capacity", got)
	}

	e.setTask(running, "completed")
	first := e.claim(e.agentA)
	if first == "" || e.claim(e.agentA) != "" {
		t.Fatalf("claimed %q then more, want exactly one run at max_concurrent_tasks = 1", first)
	}
	queued := runs.calls[0].task
	if queued == first {
		queued = runs.calls[1].task
	}
	if s := e.taskStatus(queued); s != "queued" {
		t.Fatalf("other run %s, want it still queued", s)
	}
	e.tick(d)
	if len(runs.calls) != 2 || e.openDispatches(e.agentA) != 2 {
		t.Fatalf("runs = %d, open dispatches = %d; want the queued run kept with its batch", len(runs.calls), e.openDispatches(e.agentA))
	}
	e.setTask(first, "completed")
	if got := e.claim(e.agentA); got != queued {
		t.Fatalf("claimed %q, want the queued Router run %s", got, queued)
	}
}

// TestAnAgentsAnchorRoutersRunPerSubscription: without chats each
// subscription is its own Router: both dispatch at once, and each waits only
// for its own open run.
func TestAnAgentsAnchorRoutersRunPerSubscription(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	rootA, rootB := e.issue("router a"), e.issue("router b")
	subA := e.subscribe(e.agentA, ScopeIssueTree, rootA)
	subB := e.subscribe(e.agentA, ScopeIssueTree, rootB)
	e.batchReady(subA, rootA)
	e.batchReady(subB, rootB)
	e.tick(d)
	if len(runs.calls) != 2 || runs.calls[0].issue == runs.calls[1].issue {
		t.Fatalf("runs = %+v, want one anchor run for each subscription", runs.calls)
	}
	runA := runs.calls[0]
	if runA.issue != rootA {
		runA = runs.calls[1]
	}

	e.batchReady(subA, rootA)
	e.tick(d)
	if len(runs.calls) != 2 {
		t.Fatalf("runs = %d, want subscription A's next batch to wait for its open run", len(runs.calls))
	}
	e.setTask(runA.task, "completed")
	e.tick(d)
	if len(runs.calls) != 3 || runs.calls[2].issue != rootA {
		t.Fatalf("runs = %+v, want A's next batch while B's run is open", runs.calls)
	}
}
