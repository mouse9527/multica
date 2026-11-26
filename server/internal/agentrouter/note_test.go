// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), the Router's boundary (M-112), self-contained owner decisions (M-162), the chat wakeup and batch split (M-164), and what "subscribed" means (M-175), the reply format (M-206), and wakeups on issues in scope (M-208).

package agentrouter

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildNote(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	member, agent := "member", "agent"
	var evs []batchEvent
	for i := range 200 {
		evs = append(evs, batchEvent{
			ID: fmt.Sprint(i), Kind: KindCommentCreated, IssueKey: "RT-2", IssueTitle: strings.Repeat("long title ", 20),
			ActorType: &member, ActorName: "Mouse", Payload: map[string]any{"comment_id": "c" + fmt.Sprint(i)},
			OccurredAt: now.Add(-time.Hour),
		})
	}
	evs[0] = batchEvent{ID: "s", Kind: KindCommentCreated, IssueKey: "RT-1", ActorType: &agent, ActorName: "Router", Self: true,
		Payload: map[string]any{"comment_id": "own"}, OccurredAt: now}
	evs[1] = batchEvent{ID: "st", Kind: KindStatusChanged, IssueKey: "RT-3", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"from": "todo", "to": "in_review"}, OccurredAt: now}
	evs[2] = batchEvent{ID: "f", Kind: KindTaskFailed, IssueKey: "RT-4", Payload: map[string]any{"failure_reason": "timeout"}, OccurredAt: now}

	note := buildNote(noteInput{
		sub:   dispatchSub{ID: "sub-1", StateRevision: 3, State: ""},
		batch: batch{events: evs}, anchorKey: "RT-1", scopeLabel: "issue tree RT-1",
		inflight:      []inFlightIssue{{Key: "RT-3", Title: "review me", Status: "in_review", AssigneeType: &agent, AssigneeName: "Reviewer", UpdatedAt: now.Add(-3 * time.Hour)}},
		inflightTotal: 41, now: now,
	})
	for _, want := range []string{
		"Super Agent batch for subscription sub-1 (issue tree RT-1, anchor RT-1): 199 new event(s).",
		"RT-3 status_changed todo -> in_review by member Mouse",
		"RT-4 task_failed (timeout)",
		"more: `multica router events --subscription sub-1 --state pending --output json`",
		"- RT-3 [in_review] agent Reviewer, updated 3h ago: review me",
		"(1 of 41,",
		"the rest: `multica router scope sub-1`",
		"State (revision 3)",
		"--expect-revision 3 --state-file",
		"(empty)",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q", want)
		}
	}
	if strings.Contains(note, "comment own") || strings.Contains(note, "by agent Router") {
		t.Error("note lists the agent's own event")
	}
	// The event list is capped on its own; the fixed rules around it grow as
	// rules are added, so measure only the list.
	start := strings.Index(note, "Events, oldest first:\n")
	if start < 0 {
		t.Fatal("note lacks the event list header")
	}
	events := note[start:]
	end := strings.Index(events, "\nOpen issues in scope")
	if end < 0 {
		t.Fatal("note lacks the open issues header after the event list")
	}
	events = events[:end]
	if len(events) > maxNoteEventsBytes+256 {
		t.Errorf("event list is %d bytes; it should be capped near %d", len(events), maxNoteEventsBytes)
	}
}

// Both carriers tell the Router to hand substantive work to workers and say
// where small and repository-worthy work goes (M-112).
func TestBuildNoteStatesRouterBoundary(t *testing.T) {
	member := "member"
	evs := []batchEvent{{ID: "1", Kind: KindCommentCreated, IssueKey: "RT-2", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"comment_id": "c1"}, OccurredAt: time.Now()}}
	for _, chat := range []bool{false, true} {
		note := buildNote(noteInput{sub: dispatchSub{ID: "sub-1"}, batch: batch{events: evs}, anchorKey: "RT-1",
			scopeLabel: "issue tree RT-1", now: time.Now(), chat: chat})
		for _, want := range []string{
			"you do not do the work yourself",
			"querying or analysing business data",
			"a small task becomes a local Multica issue assigned to a fitting worker",
			"also gets a GitHub issue, linked as `gh_issue` per the github-evidence skill",
			"ops, environment and credential work stays a local issue only",
			"`multica router guide`", // M-150: the full guide ships in the CLI
			"Every decision you ask of the owner must stand on its own", // M-162
			// M-164: batches, not chat wakeups, report issues in scope.
			"do not create a chat wakeup (`--until-*` condition or `--every` check) for an issue in scope",
			"A chat wakeup is still fine for issues outside the scope",
			// M-208: a wakeup on an issue in scope says what kind it is, and its turn reads current state.
			"say in your reply what kind it is",
			"a timed retry, not a repeat of the issue's events",
			"In a turn a wakeup starts, read the issue's current status and latest comments before reporting",
			// M-175: "subscribed" means a scope, not an issue subscriber.
			"\"subscribed\" (订阅) means only a scope of this Super Agent",
			"`multica router scope <subscription-id>`",
			// M-206: the reply format ships with the product, not the workspace.
			"\"每次回复的格式\" in `multica router guide`",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("chat=%v: note lacks %q", chat, want)
			}
		}
		if got := strings.Contains(note, "When the owner asks you something here"); got != chat {
			t.Errorf("chat=%v: owner-question rule present=%v", chat, got)
		}
		// M-206: an anchor run reports through `multica notify owner`, so its body
		// follows the reply format; a chat batch replies in the chat and must not
		// be pointed at notify (TestChatBoundSubscriptionDispatchesIntoItsChat).
		if got := strings.Contains(note, "Write that body as you would a reply to the owner"); got == chat {
			t.Errorf("chat=%v: notify body format rule present=%v", chat, got)
		}
		if chat && strings.Contains(note, "multica notify owner") {
			t.Error("chat batch note names `multica notify owner`")
		}
	}
}

// The reporting format came only from one workspace's notes and the agent's
// memory, so a Super Agent elsewhere answered in another format (M-206). Both
// carriers now state it.
func TestBuildNoteStatesReplyFormat(t *testing.T) {
	member := "member"
	evs := []batchEvent{{ID: "1", Kind: KindCommentCreated, IssueKey: "RT-2", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"comment_id": "c1"}, OccurredAt: time.Now()}}
	for _, chat := range []bool{false, true} {
		note := buildNote(noteInput{sub: dispatchSub{ID: "sub-1"}, batch: batch{events: evs}, anchorKey: "RT-1",
			scopeLabel: "issue tree RT-1", now: time.Now(), chat: chat})
		for _, want := range replyFormatRules {
			if !strings.Contains(note, want) {
				t.Errorf("chat=%v: note lacks %q", chat, want)
			}
		}
	}
}

// replyFormatRules are the M-206 rules every Super Agent turn is told.
var replyFormatRules = []string{
	"in the owner's language",
	"end it with a separate, numbered section of what needs the owner",
	"(「需要你处理」 in Chinese)",
	"repeat every open item each time",
	// M-288: only what the owner can act on now; later steps and notices stay out.
	"List there only what the owner can do or must decide now",
	"Leave out steps that are not the owner's turn yet, such as approving a release once another agent has tagged it",
	"say in one sentence of the body that you will ask them later",
	"Notices never go in that section: what you will do, what is arranged, who is running, when you will check again belong in the body",
	"\"none for now\" (目前没有)",
	"in the owner's time zone",
	"convert the UTC (`...Z`) times",
	"name the zone when you do not know theirs",
	"Merged is not fixed",
	"merged, not live, and still happens in production",
	"list whether to deploy as an item for the owner",
	"read its current status and latest comments",
	"A batch with nothing new for the owner must not alert them, not even to repeat open items",
	"every reply you do write, a quiet one included, still ends with all open items",
	"\"每次回复的格式\" in `multica router guide`",
}

// A quiet batch reply (router_chat_quiet, M-210) only skips the alert; it
// still ends with every open item, because the owner reads only the end of
// the last reply (M-162). The owner chose this over dropping them (M-206).
func TestQuietChatBatchStillListsOpenItems(t *testing.T) {
	member := "member"
	evs := []batchEvent{{ID: "1", Kind: KindCommentCreated, IssueKey: "RT-2", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"comment_id": "c1"}, OccurredAt: time.Now()}}
	for name, events := range map[string][]batchEvent{"empty": nil, "with events": evs} {
		in := noteInput{sub: dispatchSub{ID: "sub-1"}, batch: batch{events: events}, anchorKey: "RT-1", scopeLabel: "issue tree RT-1",
			now: time.Now(), chat: true, quiet: true}
		note := buildNote(in)
		for _, want := range []string{
			"then end it with every open item as in any reply",
			"Quiet means no alert, never fewer open items",
			"a quiet one included, still ends with all open items",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("%s: quiet chat batch note lacks %q:\n%s", name, want, note)
			}
		}
		for _, banned := range []string{"does not repeat", "gets no reply"} {
			if strings.Contains(note, banned) {
				t.Errorf("%s: quiet chat batch note still says %q:\n%s", name, banned, note)
			}
		}
	}
}
