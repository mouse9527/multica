// Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96), Router notices in the Router chat (M-111), and no late redelivery of pending notices (M-116).

package agentrouter

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// bindChat binds sub to a new active chat of its agent started by the
// subscription's owner, and returns the chat's id.
func (e *env) bindChat(sub Subscription, over ...testutil.Cols) string {
	e.t.Helper()
	chat := e.fx.ChatSession(e.t, sub.AgentID, over...)
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{AnchorChatSessionID: &chat, Change: testChange}); err != nil {
		e.t.Fatalf("bind chat: %v", err)
	}
	return chat
}

func TestChatBoundSubscriptionDispatchesIntoItsChat(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	chat := e.bindChat(sub)

	got, err := e.store.Get(e.ctx, e.ws, sub.ID)
	if err != nil || got.AnchorChatSessionID == nil || *got.AnchorChatSessionID != chat {
		t.Fatalf("subscription after bind = %+v (%v), want anchor chat %s", got.AnchorChatSessionID, err, chat)
	}

	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.calls))
	}
	run := runs.calls[0]
	if run.chat != chat || run.issue != "" || run.agent != e.agentA {
		t.Fatalf("run = %+v, want a chat run in %s", run, chat)
	}
	if !strings.HasPrefix(run.note, RouterBatchLabel+"\n") || !strings.Contains(run.note, "the owner did not") ||
		!strings.Contains(run.note, "finish without any text reply") || strings.Contains(run.note, "multica notify owner") {
		t.Fatalf("chat batch:\n%s", run.note)
	}
	// The dispatch row still names the anchor issue; its task is the chat turn.
	var anchor, task string
	if err := pool.QueryRow(e.ctx, `SELECT anchor_issue_id::text, task_id::text FROM router_dispatch WHERE subscription_id = $1`, sub.ID).Scan(&anchor, &task); err != nil {
		t.Fatal(err)
	}
	if anchor != root || task != run.task {
		t.Fatalf("dispatch row anchor %s task %s, want %s and %s", anchor, task, root, run.task)
	}

	// A failed chat turn hands its batch back like an anchor run does.
	e.setTask(run.task, "failed")
	e.tick(d)
	if len(runs.calls) != 2 || runs.calls[1].chat != chat {
		t.Fatalf("after the failed turn: runs = %+v, want the batch again in the chat", runs.calls)
	}
}

func TestChatBindingIsIgnoredWithRouterChatOff(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	chat := e.bindChat(sub)
	e.fx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, chat)

	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 || runs.calls[0].issue != root || runs.calls[0].chat != "" {
		t.Fatalf("runs = %+v, want one anchor run on %s", runs.calls, root)
	}
	if strings.Contains(runs.calls[0].note, RouterBatchLabel) {
		t.Fatalf("anchor note carries the chat label:\n%s", runs.calls[0].note)
	}
	got, _ := e.store.Get(e.ctx, e.ws, sub.ID)
	if got.PausedReason != nil {
		t.Fatalf("paused for an unused chat: %s", *got.PausedReason)
	}
}

func TestUnbindingAChatGoesBackToAnchorRuns(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)
	none := ""
	if _, err := e.store.Update(e.ctx, e.ws, sub.ID, UpdateParams{AnchorChatSessionID: &none, Change: testChange}); err != nil {
		t.Fatal(err)
	}
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	if len(runs.calls) != 1 || runs.calls[0].issue != root {
		t.Fatalf("runs = %+v, want an anchor run", runs.calls)
	}
	changes, err := e.store.ListChanges(e.ctx, e.ws, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := changes[len(changes)-1]; last.Action != ChangeUpdate || last.Detail["anchor_chat_session_id"] != "" {
		t.Fatalf("last change = %+v, want the unbind recorded", last)
	}
}

func TestBindChatValidatesTheChat(t *testing.T) {
	e := newEnv(t)
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	other := e.fx.User(t, "router other", fmt.Sprintf("router-other-%d@example.test", time.Now().UnixNano()))
	e.fx.Member(t, e.ws, other, "member")
	project := e.fx.Project(t, "router project")
	projSub := e.subscribe(e.agentB, ScopeProject, project)

	for name, tc := range map[string]struct {
		sub  Subscription
		chat string
		want string
	}{
		"another agent's chat":   {sub, e.fx.ChatSession(t, e.agentB), "another agent"},
		"someone else's chat":    {sub, e.fx.ChatSession(t, e.agentA, testutil.Cols{"creator_id": other}), "someone other than"},
		"archived chat":          {sub, e.fx.ChatSession(t, e.agentA, testutil.Cols{"status": "archived"}), "archived"},
		"unknown chat":           {sub, "00000000-0000-0000-0000-0000000000c1", "not found"},
		"no anchor issue to log": {projSub, e.fx.ChatSession(t, e.agentB), "needs an anchor issue"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.store.Update(e.ctx, e.ws, tc.sub.ID, UpdateParams{AnchorChatSessionID: &tc.chat, Change: testChange})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid mentioning %q", err, tc.want)
			}
			got, _ := e.store.Get(e.ctx, e.ws, tc.sub.ID)
			if got.AnchorChatSessionID != nil {
				t.Fatalf("bound anyway to %s", *got.AnchorChatSessionID)
			}
		})
	}
}

func TestUnusableChatPausesDispatchAndTellsTheOwner(t *testing.T) {
	for name, breakChat := range map[string]func(e *env, chat string){
		"archived": func(e *env, chat string) {
			e.fx.Exec(e.t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, chat)
		},
		"deleted": func(e *env, chat string) { e.fx.Exec(e.t, `DELETE FROM chat_session WHERE id = $1`, chat) },
		"owner left": func(e *env, chat string) {
			e.fx.Exec(e.t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, e.ws, e.user)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, d, runs := newDispatchEnv(t)
			d.Chat = true
			root := e.issue("router root")
			sub := e.subscribe(e.agentA, ScopeIssueTree, root)
			chat := e.bindChat(sub)
			breakChat(e, chat)

			// No event is pending: the pause does not wait for a batch.
			e.tick(d)
			got, err := e.store.Get(e.ctx, e.ws, sub.ID)
			if err != nil || got.PausedReason == nil || *got.PausedReason != PausedAgentUnavailable || !got.Enabled {
				t.Fatalf("subscription = %+v (%v), want dispatch paused, capture on", got, err)
			}
			var title, body string
			if err := pool.QueryRow(e.ctx, `
				SELECT title, body FROM owner_notice WHERE source_subscription_id = $1 AND recipient_user_id = $2`,
				sub.ID, e.user).Scan(&title, &body); err != nil {
				t.Fatalf("owner notice: %v", err)
			}
			if !strings.Contains(body, chat) || !strings.Contains(body, "--anchor-chat") {
				t.Fatalf("notice %q: %s", title, body)
			}
			// The chat cannot carry it: the inbox does.
			if len(runs.notices) != 0 {
				t.Fatalf("notice posted into an unusable chat: %+v", runs.notices)
			}
			// A member who left the workspace has no inbox there: the notice
			// is kept, and its inbox delivery recorded as skipped.
			want := 1
			if name == "owner left" {
				want = 0
			}
			var inbox int
			if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND type = $2`, e.ws, InboxNoticeType).Scan(&inbox); err != nil || inbox != want {
				t.Fatalf("inbox items = %d (%v), want %d", inbox, err, want)
			}

			// Events keep being captured but start nothing.
			e.publishComment(e.comment(root))
			e.age(sub.ID)
			e.tick(d)
			if len(runs.calls) != 0 {
				t.Fatalf("dispatched into an unusable chat: %+v", runs.calls)
			}
		})
	}
}

// TestChatUnavailableOnEnqueuePauses covers the race the scan cannot: the
// chat stops taking batches between the scan and the enqueue.
func TestChatUnavailableOnEnqueuePauses(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	e.bindChat(sub)
	runs.err = fmt.Errorf("%w: chat task: session archived", ErrChatUnavailable)
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.tick(d)
	got, _ := e.store.Get(e.ctx, e.ws, sub.ID)
	if got.PausedReason == nil || *got.PausedReason != PausedAgentUnavailable {
		t.Fatalf("paused_reason = %v, want agent_unavailable", got.PausedReason)
	}
	if p := e.pending(sub.ID); len(p) != 1 {
		t.Fatalf("pending = %d, want the event kept", len(p))
	}
}

// TestPauseNoticeGoesToTheRouterChat: a pause whose cause leaves the chat
// usable is told in the chat, with the backlog and the way out; the inbox
// does not repeat it.
func TestPauseNoticeGoesToTheRouterChat(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	chat := e.bindChat(sub)
	e.publishComment(e.comment(root))
	e.publishComment(e.comment(root))
	e.age(sub.ID)
	e.fx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, e.agentA)
	e.tick(d)

	if r := e.get(sub.ID).PausedReason; r == nil || *r != PausedAgentUnavailable {
		t.Fatalf("paused_reason = %v, want agent_unavailable", r)
	}
	if len(runs.calls) != 0 || len(runs.notices) != 1 {
		t.Fatalf("runs = %d, chat notices = %d; want 0 and 1", len(runs.calls), len(runs.notices))
	}
	got := runs.notices[0]
	if got.chat != chat || !strings.HasPrefix(got.note, RouterNoticeLabel+"\n") || !strings.Contains(got.note, "Router paused") ||
		!strings.Contains(got.note, "no runtime") || !strings.Contains(got.note, "2 event(s) are waiting") ||
		!strings.Contains(got.note, "multica router resume "+sub.ID) {
		t.Fatalf("chat notice = %+v", got)
	}
	notices, _ := d.Notices.ListNotices(e.ctx, NoticeFilter{WorkspaceID: e.ws, SubscriptionID: sub.ID})
	if len(notices) != 1 || notices[0].ChannelStates["chat"].State != DeliverySent || notices[0].ChannelStates["chat"].Ref != "message-1" ||
		notices[0].ChannelStates["inbox"].State != DeliverySkipped {
		t.Fatalf("notices = %+v", notices)
	}
	var inbox int
	if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1`, e.ws).Scan(&inbox); err != nil || inbox != 0 {
		t.Fatalf("inbox items = %d (%v), want none", inbox, err)
	}

	// A chat started by someone else than the notice's recipient never gets it.
	other := e.fx.User(t, "router other", fmt.Sprintf("router-other-%d@example.test", time.Now().UnixNano()))
	e.fx.Exec(t, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, chat, other)
	n, _, err := d.Notices.Send(e.ctx, NoticeInput{
		WorkspaceID: e.ws, RecipientUserID: e.user, Title: "t", SenderType: "system", SourceSubscriptionID: sub.ID,
	})
	if err != nil || n.ChannelStates["chat"].State != DeliverySkipped || n.ChannelStates["inbox"].State != DeliverySent {
		t.Fatalf("notice = %+v (%v), want it in the inbox only", n, err)
	}
}

// TestPendingNoticesAreNotRedeliveredLate: the next pass delivers a notice
// no channel has attempted yet, but not one an older release already sent
// to the inbox (the chat channel did not exist then), nor one past
// PendingNoticeWindow, whose state may be over.
func TestPendingNoticesAreNotRedeliveredLate(t *testing.T) {
	e, d, runs := newDispatchEnv(t)
	d.Chat = true
	root := e.issue("router root")
	sub := e.subscribe(e.agentA, ScopeIssueTree, root)
	chat := e.bindChat(sub)
	write := func(key string) Notice {
		t.Helper()
		n, _, err := insertNotice(e.ctx, pool, NoticeInput{
			WorkspaceID: e.ws, RecipientUserID: e.user, Title: "Router paused " + key, Urgency: UrgencyHigh,
			DedupeKey: key, SenderType: "system", SourceSubscriptionID: sub.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	sent := write("sent-before")
	e.fx.Exec(t, `UPDATE owner_notice SET channel_states = '{"inbox":{"state":"sent","ref":"item-1"}}' WHERE id = $1`, sent.ID)
	stale := write("stale")
	e.fx.Exec(t, `UPDATE owner_notice SET created_at = now() - interval '20 minutes' WHERE id = $1`, stale.ID)
	fresh := write("fresh")

	if _, err := d.Notices.DeliverPending(e.ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	if len(runs.notices) != 1 || runs.notices[0].chat != chat || !strings.Contains(runs.notices[0].note, fresh.Title) {
		t.Fatalf("chat notices = %+v, want only %q", runs.notices, fresh.Title)
	}
	if got, _ := d.Notices.Get(e.ctx, e.ws, fresh.ID); got.ChannelStates["chat"].State != DeliverySent ||
		got.ChannelStates["inbox"].State != DeliverySkipped {
		t.Fatalf("fresh notice = %+v", got.ChannelStates)
	}
	if got, _ := d.Notices.Get(e.ctx, e.ws, sent.ID); len(got.ChannelStates) != 1 || got.ChannelStates["inbox"].State != DeliverySent {
		t.Fatalf("notice already sent to the inbox = %+v, want it left alone", got.ChannelStates)
	}
	if got, _ := d.Notices.Get(e.ctx, e.ws, stale.ID); len(got.ChannelStates) != 0 {
		t.Fatalf("stale notice = %+v, want it left alone", got.ChannelStates)
	}
}
