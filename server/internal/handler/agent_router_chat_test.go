// Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96),
// Router page settings (M-105), and Router notices in the Router chat (M-111).

package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func withRouterChatFlags(t *testing.T, chat bool) {
	t.Helper()
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	provider.Set(featureflags.RouterDispatch, featureflag.Rule{Default: true})
	provider.Set(featureflags.RouterChat, featureflag.Rule{Default: chat})
	orig := testHandler.FeatureFlags
	testHandler.FeatureFlags = featureflag.NewService(provider)
	t.Cleanup(func() { testHandler.FeatureFlags = orig })
}

func cleanupRouterChat(t *testing.T, subID string) {
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM router_chat_anchor WHERE subscription_id = $1`,
			`DELETE FROM router_dispatch WHERE subscription_id = $1`,
			`DELETE FROM router_event WHERE subscription_id = $1`,
			`DELETE FROM router_issue_seen WHERE subscription_id = $1`,
			`DELETE FROM router_subscription_change WHERE subscription_id = $1`,
			`DELETE FROM owner_notice WHERE source_subscription_id = $1`,
			`DELETE FROM router_subscription WHERE id = $1`,
		} {
			dbfx.Exec(t, q, subID)
		}
	})
}

func TestRouterAnchorChatAPI(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	withRouterChatFlags(t, false)
	var sub map[string]any
	routerCall(t, newRequest("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": f.agentA, "scope_type": "issue_tree", "scope_id": f.root,
	})).Want(http.StatusCreated).JSON(&sub)
	id := sub["id"].(string)
	cleanupRouterChat(t, id)
	if v, ok := sub["anchor_chat_session_id"]; !ok || v != nil {
		t.Fatalf("new subscription anchor_chat_session_id = %v (present %v), want null", v, ok)
	}
	chat := dbfx.ChatSession(t, f.agentA)

	// router_chat off: the field is refused, nothing is bound.
	routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": chat})).
		Want(http.StatusBadRequest)

	withRouterChatFlags(t, true)
	routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": chat})).
		Want(http.StatusOK).JSON(&sub)
	if sub["anchor_chat_session_id"] != chat {
		t.Fatalf("bound = %v", sub)
	}
	for name, ref := range map[string]string{
		"not a uuid":           "M-1",
		"archived chat":        dbfx.ChatSession(t, f.agentA, testutil.Cols{"status": "archived"}),
		"another agent's chat": dbfx.ChatSession(t, f.agentB),
		"someone else's chat":  dbfx.ChatSession(t, f.agentA, testutil.Cols{"creator_id": f.member}),
	} {
		t.Run(name, func(t *testing.T) {
			routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": ref})).
				Want(http.StatusBadRequest)
		})
	}
	routerCall(t, newRequest("PATCH", "/api/router/subscriptions/"+id, map[string]any{"anchor_chat_session_id": "none"})).
		Want(http.StatusOK).JSON(&sub)
	if sub["anchor_chat_session_id"] != nil {
		t.Fatalf("after none = %v", sub)
	}
}

// TestRouterCreateBoundToChat: the settings page creates a subscription
// already bound to its chat, in one transaction, so it never dispatches an
// anchor run in between. A binding that fails validation creates nothing.
func TestRouterCreateBoundToChat(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	cleanupRouterS2(t, f)
	chat := dbfx.ChatSession(t, f.agentA)
	project := dbfx.Project(t, "router create bound project")
	create := func(body map[string]any) *testutil.Response {
		body["agent_id"] = f.agentA
		body["anchor_chat_session_id"] = chat
		return routerCall(t, newRequest("POST", "/api/router/subscriptions", body))
	}
	countSubs := func() int {
		var n int
		if err := testPool.QueryRow(context.Background(),
			`SELECT count(*) FROM router_subscription WHERE agent_id = $1`, f.agentA).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	withRouterChatFlags(t, false)
	create(map[string]any{"scope_type": "issue_tree", "scope_id": f.root}).Want(http.StatusBadRequest)

	withRouterChatFlags(t, true)
	for name, body := range map[string]map[string]any{
		// A project scope has no default anchor issue; a bound one needs it.
		"project without anchor": {"scope_type": "project", "scope_id": project},
		"another agent's chat":   {"scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": dbfx.ChatSession(t, f.agentB)},
		"archived chat":          {"scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": dbfx.ChatSession(t, f.agentA, testutil.Cols{"status": "archived"})},
		"someone else's chat":    {"scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": dbfx.ChatSession(t, f.agentA, testutil.Cols{"creator_id": f.member})},
		"not a uuid":             {"scope_type": "issue_tree", "scope_id": f.root, "anchor_chat_session_id": "M-1"},
	} {
		t.Run(name, func(t *testing.T) {
			req := map[string]any{"agent_id": f.agentA, "anchor_chat_session_id": chat}
			for k, v := range body {
				req[k] = v
			}
			routerCall(t, newRequest("POST", "/api/router/subscriptions", req)).Want(http.StatusBadRequest)
			if n := countSubs(); n != 0 {
				t.Fatalf("a refused binding left %d subscriptions", n)
			}
		})
	}

	var tree, proj map[string]any
	create(map[string]any{"scope_type": "issue_tree", "scope_id": f.root}).Want(http.StatusCreated).JSON(&tree)
	cleanupRouterChat(t, tree["id"].(string))
	if tree["anchor_chat_session_id"] != chat || tree["anchor_issue_id"] != f.root {
		t.Fatalf("tree subscription = %v", tree)
	}
	create(map[string]any{"scope_type": "project", "scope_id": project, "anchor_issue_id": f.child}).
		Want(http.StatusCreated).JSON(&proj)
	cleanupRouterChat(t, proj["id"].(string))
	if proj["anchor_chat_session_id"] != chat || proj["anchor_issue_id"] != f.child {
		t.Fatalf("project subscription = %v", proj)
	}

	var detail string
	if err := testPool.QueryRow(context.Background(), `
		SELECT detail->>'anchor_chat_session_id' FROM router_subscription_change
		WHERE subscription_id = $1 AND action = 'create'`, tree["id"]).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != chat {
		t.Fatalf("create audit detail anchor_chat_session_id = %q, want %q", detail, chat)
	}
}

// routerChat is a chat-bound subscription on a daemon-backed agent, with one
// due event from the owner, and a dispatcher with router_chat on.
type routerChat struct {
	agentID, sessionID, runtimeID, daemonID string
	d                                       *agentrouter.Dispatcher
}

func newRouterChat(t *testing.T, ctx context.Context) routerChat {
	t.Helper()
	var rc routerChat
	rc.agentID, rc.sessionID, rc.runtimeID, rc.daemonID = setupDirectChatSession(t, ctx, "router chat")
	root := dbfx.Issue(t, "router chat root")
	store := agentrouter.NewStore(testPool, testPool)
	types, _ := agentrouter.NormalizeEventTypes(nil)
	sub, err := store.Create(ctx, agentrouter.CreateParams{
		WorkspaceID: testWorkspaceID, AgentID: rc.agentID, OwnerUserID: testUserID,
		ScopeType: agentrouter.ScopeIssueTree, ScopeID: root, EventTypes: types,
		CreatedByType: "member", CreatedByID: testUserID,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRouterChat(t, sub.ID)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, rc.sessionID)
		dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE agent_id = $1`, rc.agentID)
	})
	if _, err := store.Update(ctx, testWorkspaceID, sub.ID, agentrouter.UpdateParams{
		AnchorChatSessionID: &rc.sessionID, Change: agentrouter.Change{ActorType: "system"},
	}); err != nil {
		t.Fatalf("bind chat: %v", err)
	}
	dbfx.Exec(t, `
		INSERT INTO router_event (workspace_id, subscription_id, agent_id, event_key, kind, issue_id, actor_type, actor_id, source, occurred_at, created_at)
		VALUES ($1, $2, $3, 'comment:router-chat-test', 'comment_created', $4, 'member', $5, 'bus', now(), now() - interval '1 hour')`,
		testWorkspaceID, sub.ID, rc.agentID, root, testUserID)
	rc.d = agentrouter.NewDispatcher(store, agentrouter.TaskRuns{Tasks: testHandler.TaskService},
		agentrouter.NewNotifier(store, agentrouter.InboxAdapter{Queries: db.New(testPool), Bus: testHandler.Bus}))
	rc.d.Chat = true
	return rc
}

// TestRouterChatBatchThroughTheRealChat goes through the real task service
// and daemon claim: a batch lands in the chat as a router_batch input, a
// queued owner message is claimed before it, the agent receives the batch as
// its prompt, and a batch turn without text stays silent.
func TestRouterChatBatchThroughTheRealChat(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rc := newRouterChat(t, ctx)
	agentID, sessionID, runtimeID, daemonID, d := rc.agentID, rc.sessionID, rc.runtimeID, rc.daemonID, rc.d

	var mu sync.Mutex
	var done []protocol.ChatDonePayload
	var batchEvents []protocol.ChatMessagePayload
	testHandler.Bus.Subscribe(protocol.EventChatDone, func(e events.Event) {
		if p, ok := e.Payload.(protocol.ChatDonePayload); ok && p.ChatSessionID == sessionID {
			mu.Lock()
			done = append(done, p)
			mu.Unlock()
		}
	})
	testHandler.Bus.Subscribe(protocol.EventChatMessage, func(e events.Event) {
		if p, ok := e.Payload.(protocol.ChatMessagePayload); ok && p.ChatSessionID == sessionID && e.ActorType == "system" {
			mu.Lock()
			batchEvents = append(batchEvents, p)
			mu.Unlock()
		}
	})

	if _, err := d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var batchTask, kind, content, initiator string
	var priority int
	var originatorSet bool
	if err := testPool.QueryRow(ctx, `
		SELECT t.id::text, t.priority, t.originator_user_id IS NOT NULL, t.initiator_user_id::text, m.message_kind, m.content
		FROM agent_task_queue t JOIN chat_message m ON m.task_id = t.id
		WHERE t.chat_session_id = $1 AND t.chat_input_task_id = t.id`, sessionID,
	).Scan(&batchTask, &priority, &originatorSet, &initiator, &kind, &content); err != nil {
		t.Fatalf("batch turn: %v", err)
	}
	if priority != service.RouterChatPriority || originatorSet || initiator != testUserID || kind != protocol.ChatMessageKindRouterBatch ||
		!strings.HasPrefix(content, agentrouter.RouterBatchLabel+"\n") || !strings.Contains(content, "comment_created") {
		t.Fatalf("batch turn: priority %d, originator set %v, initiator %s, kind %s, content:\n%s", priority, originatorSet, initiator, kind, content)
	}
	mu.Lock()
	if len(batchEvents) != 1 || batchEvents[0].MessageID != "" || batchEvents[0].TaskID != batchTask {
		t.Fatalf("chat:message events = %+v, want one without a message id", batchEvents)
	}
	mu.Unlock()

	// The owner speaks after the batch was queued; the owner is claimed first.
	ownerTask := sendDirectChat(t, ctx, agentID, sessionID, "what is going on?")
	claimed := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if claimed.ChatMessage != "what is going on?" {
		t.Fatalf("first claim delivered %q, want the owner's message", claimed.ChatMessage)
	}
	markTaskRunning(t, ctx, ownerTask)
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(ownerTask), completeResult(t, "RT is fine"), "", "", "", false, "", ""); err != nil {
		t.Fatalf("complete owner turn: %v", err)
	}

	// Then the batch, delivered as the prompt.
	claimed = claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if !strings.HasPrefix(claimed.ChatMessage, agentrouter.RouterBatchLabel) {
		t.Fatalf("second claim delivered %q, want the batch", claimed.ChatMessage)
	}
	markTaskRunning(t, ctx, batchTask)
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(batchTask), completeResult(t, ""), "", "", "", false, "", ""); err != nil {
		t.Fatalf("complete batch turn: %v", err)
	}
	mu.Lock()
	var batchDone *protocol.ChatDonePayload
	for i := range done {
		if done[i].TaskID == batchTask {
			batchDone = &done[i]
		}
	}
	if batchDone == nil || batchDone.MessageKind != protocol.ChatMessageKindNoResponse || batchDone.Notification != nil {
		t.Fatalf("batch chat:done = %+v, want a silent no_response", batchDone)
	}
	mu.Unlock()

	// The session list neither previews the batch or its silent turn nor
	// counts the silent turn as unread: the owner's reply is the latest.
	rows, err := testHandler.Queries.ListChatSessionsByCreator(ctx, db.ListChatSessionsByCreatorParams{
		WorkspaceID: parseUUID(testWorkspaceID), CreatorID: parseUUID(testUserID),
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if uuidToString(r.ID) != sessionID {
			continue
		}
		found = true
		if r.LastMessageContent != "RT is fine" || r.LastMessageKind != protocol.ChatMessageKindMessage || r.UnreadCount != 1 {
			t.Fatalf("session row: last %q (%s), unread %d; want the owner's reply and 1 unread", r.LastMessageContent, r.LastMessageKind, r.UnreadCount)
		}
	}
	if !found {
		t.Fatal("session missing from the list")
	}

	// The batch settles like an anchor run.
	if _, err := d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("settle: %v", err)
	}
	var outcome string
	if err := testPool.QueryRow(ctx, `SELECT COALESCE(outcome, '') FROM router_dispatch WHERE task_id = $1`, batchTask).Scan(&outcome); err != nil || outcome != "completed" {
		t.Fatalf("dispatch outcome = %q (%v), want completed", outcome, err)
	}
}

// TestRouterPauseNoticeThroughTheRealChat: with the chat adapter, a pause
// whose cause leaves the chat usable lands in the chat as an ordinary
// assistant message without a task, raises the chat reply alert for the
// owner, counts as unread, and does not also go to the inbox.
func TestRouterPauseNoticeThroughTheRealChat(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rc := newRouterChat(t, ctx)
	runs := agentrouter.TaskRuns{Tasks: testHandler.TaskService}
	rc.d.Notices = agentrouter.NewNotifier(rc.d.Store, agentrouter.ChatAdapter{Store: rc.d.Store, Poster: runs},
		agentrouter.InboxAdapter{Queries: db.New(testPool), Bus: testHandler.Bus})
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1 AND type = $2`, testWorkspaceID, agentrouter.InboxNoticeType)
	})

	var mu sync.Mutex
	var alerts []events.Event
	var messages []protocol.ChatMessagePayload
	testHandler.Bus.Subscribe(protocol.EventChatReplyNotification, func(e events.Event) {
		if e.ChatSessionID == rc.sessionID {
			mu.Lock()
			alerts = append(alerts, e)
			mu.Unlock()
		}
	})
	testHandler.Bus.Subscribe(protocol.EventChatMessage, func(e events.Event) {
		if p, ok := e.Payload.(protocol.ChatMessagePayload); ok && p.ChatSessionID == rc.sessionID {
			mu.Lock()
			messages = append(messages, p)
			mu.Unlock()
		}
	})

	dbfx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, rc.agentID)
	if _, err := rc.d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var msgID, role, kind, content string
	var hasTask bool
	if err := testPool.QueryRow(ctx, `
		SELECT id::text, role, message_kind, content, task_id IS NOT NULL FROM chat_message WHERE chat_session_id = $1`, rc.sessionID,
	).Scan(&msgID, &role, &kind, &content, &hasTask); err != nil {
		t.Fatalf("chat notice: %v", err)
	}
	if role != "assistant" || kind != protocol.ChatMessageKindMessage || hasTask ||
		!strings.HasPrefix(content, agentrouter.RouterNoticeLabel+"\n") || !strings.Contains(content, "Router paused") ||
		!strings.Contains(content, "1 event(s) are waiting") {
		t.Fatalf("chat notice: role %s kind %s task %v content:\n%s", role, kind, hasTask, content)
	}
	mu.Lock()
	if len(messages) != 1 || messages[0].MessageID != msgID || messages[0].Role != "assistant" {
		t.Fatalf("chat:message events = %+v", messages)
	}
	if len(alerts) != 1 || alerts[0].ChatSessionOwnerID != testUserID || alerts[0].WorkspaceID != testWorkspaceID {
		t.Fatalf("chat reply alerts = %+v", alerts)
	}
	if p, ok := alerts[0].Payload.(protocol.ChatReplyNotificationPayload); !ok || p.MessageID != msgID || p.Title == "" ||
		!strings.Contains(p.Body, "Router paused") {
		t.Fatalf("alert payload = %+v", alerts[0].Payload)
	}
	mu.Unlock()

	var chatState, inboxState string
	if err := testPool.QueryRow(ctx, `
		SELECT channel_states->'chat'->>'state', channel_states->'inbox'->>'state' FROM owner_notice
		WHERE source_subscription_id = (SELECT subscription_id FROM router_chat_anchor WHERE chat_session_id = $1)`, rc.sessionID,
	).Scan(&chatState, &inboxState); err != nil {
		t.Fatal(err)
	}
	if chatState != agentrouter.DeliverySent || inboxState != agentrouter.DeliverySkipped {
		t.Fatalf("channel states: chat %s inbox %s", chatState, inboxState)
	}

	rows, err := testHandler.Queries.ListChatSessionsByCreator(ctx, db.ListChatSessionsByCreatorParams{
		WorkspaceID: parseUUID(testWorkspaceID), CreatorID: parseUUID(testUserID),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if uuidToString(r.ID) == rc.sessionID && (r.LastMessageContent != content || r.UnreadCount != 1) {
			t.Fatalf("session row: last %q, unread %d; want the notice, unread", r.LastMessageContent, r.UnreadCount)
		}
	}
}

// TestRouterChatFailedBatchNeverJumpsTheOwner: a batch turn that fails with
// a retryable reason (timeout: its retry would be queued at once) gets no
// system retry (a chat retry would be raised to
// priority 3, above the owner's 2); dispatch puts the batch back and writes
// it into the chat again at priority 1, and the owner's queued message is
// still claimed first.
func TestRouterChatFailedBatchNeverJumpsTheOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rc := newRouterChat(t, ctx)
	if _, err := rc.d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	claimed := claimTaskForRuntimeGuard(t, rc.runtimeID, rc.daemonID)
	if !strings.HasPrefix(claimed.ChatMessage, agentrouter.RouterBatchLabel) {
		t.Fatalf("claimed %q, want the batch", claimed.ChatMessage)
	}
	var first string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'dispatched'`, rc.sessionID,
	).Scan(&first); err != nil {
		t.Fatalf("batch turn: %v", err)
	}
	markTaskRunning(t, ctx, first)

	ownerTask := sendDirectChat(t, ctx, rc.agentID, rc.sessionID, "anything for me?")
	if _, err := testHandler.TaskService.FailTask(ctx, parseUUID(first), "run timed out", "", "", "", "timeout", false, "", ""); err != nil {
		t.Fatalf("fail batch turn: %v", err)
	}
	var retries int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id = $1`, first).Scan(&retries); err != nil || retries != 0 {
		t.Errorf("system retries of the batch turn = %d (%v), want none", retries, err)
	}

	// Settle hands the batch back and dispatch writes it into the chat again.
	if _, err := rc.d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var second string
	var priority int
	if err := testPool.QueryRow(ctx, `
		SELECT t.id::text, t.priority FROM agent_task_queue t JOIN chat_message m ON m.task_id = t.id
		WHERE t.chat_session_id = $1 AND m.message_kind = 'router_batch' AND t.id <> $2`, rc.sessionID, first,
	).Scan(&second, &priority); err != nil {
		t.Errorf("re-dispatched batch: %v", err)
	} else if priority != service.RouterChatPriority {
		t.Errorf("re-dispatched batch priority = %d, want %d", priority, service.RouterChatPriority)
	}

	claimed = claimTaskForRuntimeGuard(t, rc.runtimeID, rc.daemonID)
	if claimed.ChatMessage != "anything for me?" {
		t.Fatalf("claimed %q after the failed batch, want the owner's message (task %s)", claimed.ChatMessage, ownerTask)
	}
	var above int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue t
		WHERE t.chat_session_id = $1 AND t.priority >= 2
		  AND EXISTS (SELECT 1 FROM chat_message m WHERE m.task_id = COALESCE(t.chat_input_task_id, t.id) AND m.message_kind = 'router_batch')`,
		rc.sessionID).Scan(&above); err != nil || above != 0 {
		t.Fatalf("batch turns at or above the owner's priority = %d (%v), want 0", above, err)
	}
}

// TestRouterChatKeepsThePrivateProjectFence: a batch carries issue titles and
// comment payloads, so a chat turn is refused wherever an anchor run on the
// same anchor would be (MUL-8472), and also when the member whose chat it is
// cannot see the project. A refusal pauses dispatch and writes no batch
// message and no task.
func TestRouterChatKeepsThePrivateProjectFence(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	outsider := dbfx.User(t, "router chat outsider", fmt.Sprintf("router-chat-outsider-%d@example.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, outsider, "member")
	private := dbfx.Project(t, "router chat private", testutil.Cols{"visibility": "private"})
	runtime := dbfx.Runtime(t, "router chat private runtime")
	store := agentrouter.NewStore(testPool, testPool)

	for _, tc := range []struct {
		name        string
		agentOwner  string // cannot see the project when it is the outsider
		chatOwner   string // the subscription's owner and the chat's creator
		checkAnchor bool
	}{
		{name: "agent owner cannot see the project", agentOwner: outsider, chatOwner: testUserID, checkAnchor: true},
		{name: "chat owner cannot see the project", agentOwner: testUserID, chatOwner: outsider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := dbfx.Agent(t, "router chat private agent", runtime, testutil.Cols{"owner_id": tc.agentOwner})
			root := dbfx.Issue(t, "router chat private root", testutil.Cols{"project_id": private})
			chat := dbfx.ChatSession(t, agent, testutil.Cols{"creator_id": tc.chatOwner})
			types, _ := agentrouter.NormalizeEventTypes(nil)
			sub, err := store.Create(ctx, agentrouter.CreateParams{
				WorkspaceID: testWorkspaceID, AgentID: agent, OwnerUserID: tc.chatOwner,
				ScopeType: agentrouter.ScopeIssueTree, ScopeID: root, EventTypes: types,
				CreatedByType: "member", CreatedByID: tc.chatOwner,
			})
			if err != nil {
				t.Fatal(err)
			}
			cleanupRouterChat(t, sub.ID)
			t.Cleanup(func() {
				dbfx.Exec(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, chat)
				dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agent)
				dbfx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1 AND type = $2`, testWorkspaceID, agentrouter.InboxNoticeType)
			})
			system := agentrouter.Change{ActorType: "system"}
			if _, err := store.Update(ctx, testWorkspaceID, sub.ID, agentrouter.UpdateParams{AnchorChatSessionID: &chat, Change: system}); err != nil {
				t.Fatalf("bind chat: %v", err)
			}
			dbfx.Exec(t, `
				INSERT INTO router_event (workspace_id, subscription_id, agent_id, event_key, kind, issue_id, source, occurred_at, created_at)
				VALUES ($1, $2, $3, 'comment:router-chat-private', 'comment_created', $4, 'bus', now(), now() - interval '1 hour')`,
				testWorkspaceID, sub.ID, agent, root)
			d := agentrouter.NewDispatcher(store, agentrouter.TaskRuns{Tasks: testHandler.TaskService},
				agentrouter.NewNotifier(store, agentrouter.InboxAdapter{Queries: db.New(testPool), Bus: testHandler.Bus}))
			refused := func(carrier string) {
				t.Helper()
				if _, err := d.TickWorkspace(ctx, testWorkspaceID); err != nil {
					t.Fatalf("%s dispatch: %v", carrier, err)
				}
				got, err := store.Get(ctx, testWorkspaceID, sub.ID)
				if err != nil || got.PausedReason == nil || *got.PausedReason != agentrouter.PausedAgentUnavailable {
					t.Fatalf("%s: subscription = %+v (%v), want dispatch paused", carrier, got.PausedReason, err)
				}
				var tasks, messages int
				if err := testPool.QueryRow(ctx, `
					SELECT (SELECT count(*) FROM agent_task_queue WHERE agent_id = $1),
					       (SELECT count(*) FROM chat_message WHERE chat_session_id = $2)`, agent, chat).Scan(&tasks, &messages); err != nil {
					t.Fatal(err)
				}
				if tasks != 0 || messages != 0 {
					t.Fatalf("%s: %d task(s), %d chat message(s) written, want none", carrier, tasks, messages)
				}
			}

			if tc.checkAnchor {
				refused("anchor run")
				if _, err := store.Resume(ctx, testWorkspaceID, sub.ID, system); err != nil {
					t.Fatal(err)
				}
			}
			d.Chat = true
			refused("chat")

			_, err = agentrouter.TaskRuns{Tasks: testHandler.TaskService}.EnqueueRouterChatRun(ctx, chat, agent, root, "[Router 事件批次 · 非 Owner 发言]\nprobe")
			if !errors.Is(err, agentrouter.ErrRunRefused) || !strings.Contains(err.Error(), service.ErrProjectAccessDenied.Error()) {
				t.Fatalf("direct enqueue err = %v, want ErrRunRefused for the private project", err)
			}
		})
	}
}
