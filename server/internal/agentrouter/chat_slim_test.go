// Added by the mouse9527/multica fork: chat batches without the rules every chat turn already carries (M-240).

package agentrouter

import (
	"strings"
	"testing"
	"time"
)

// turnNoteRules are the rules a chat turn's Super Agent note carries and a
// slim chat batch leaves out.
var turnNoteRules = map[string]string{
	"routerSubscribedMeans": routerSubscribedMeans,
	"routerWakeupSplit":     routerWakeupSplit,
	"routerOwnerDecisions":  routerOwnerDecisions,
	"routerReplyFormat":     routerReplyFormat,
}

func slimTestInput(chat, quiet, slim bool) noteInput {
	member := "member"
	evs := []batchEvent{{ID: "1", Kind: KindCommentCreated, IssueKey: "RT-2", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"comment_id": "c1"}, OccurredAt: time.Now()}}
	return noteInput{sub: dispatchSub{ID: "sub-1"}, batch: batch{events: evs}, anchorKey: "RT-1",
		scopeLabel: "issue tree RT-1", now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), chat: chat, quiet: quiet, slim: slim}
}

// With slim on, a chat batch keeps its opening, the reply rule, the boundary
// and a pointer, and leaves out the four rules; the turn note the same batch
// turn gets still has every one of them, so no rule is lost (M-240).
func TestSlimChatBatchLeavesOutTheTurnNoteRules(t *testing.T) {
	turnNote := buildChatTurnNote([]chatTurnScope{{SubscriptionID: "sub-1", Label: "issue tree RT-1", Enabled: true}})
	for _, quiet := range []bool{false, true} {
		note := buildNote(slimTestInput(true, quiet, true))
		for name, rule := range turnNoteRules {
			if strings.Contains(note, rule) {
				t.Errorf("quiet=%v: slim chat batch still carries %s", quiet, name)
			}
			if !strings.Contains(turnNote, rule) {
				t.Errorf("chat turn note lacks %s:\n%s", name, turnNote)
			}
		}
		for _, banned := range []string{
			"\"subscribed\" (订阅) means only a scope",
			"do not create a chat wakeup",
			"Every decision you ask of the owner must stand on its own",
			"\"每次回复的格式\" in `multica router guide`",
		} {
			if strings.Contains(note, banned) {
				t.Errorf("quiet=%v: slim chat batch still says %q", quiet, banned)
			}
		}
		for _, want := range []string{
			RouterBatchLabel + "\n",
			"The Super Agent dispatcher wrote this message into your chat with the owner; the owner did not.",
			chatBatchReplyRule(quiet),
			routerBoundaryCore,
			chatBatchRulesPointer,
			"When the owner asks you something here",
			"Events, oldest first:\n",
			"Open issues in scope",
			"State (revision 0)",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("quiet=%v: slim chat batch lacks %q:\n%s", quiet, want, note)
			}
		}
	}
}

// Slim on changes exactly one thing in a chat batch: the four rules become
// the pointer. Off, the batch is the one before slim existed, byte for byte:
// the reply rule, then routerBoundary in full, then the owner-question rule.
func TestSlimChatBatchOffIsUnchanged(t *testing.T) {
	four := routerSubscribedMeans + routerWakeupSplit + routerOwnerDecisions + routerReplyFormat
	if routerBoundary != routerBoundaryCore+four {
		t.Fatal("routerBoundary is no longer its core followed by the four rules")
	}
	for _, quiet := range []bool{false, true} {
		off := buildNote(slimTestInput(true, quiet, false))
		if !strings.Contains(off, chatBatchReplyRule(quiet)+routerBoundary+"When the owner asks you something here") {
			t.Errorf("quiet=%v: chat batch with slim off lost its rules:\n%s", quiet, off)
		}
		if strings.Contains(off, chatBatchRulesPointer) {
			t.Errorf("quiet=%v: chat batch with slim off carries the pointer", quiet)
		}
		on := buildNote(slimTestInput(true, quiet, true))
		if got := strings.Replace(off, four, chatBatchRulesPointer, 1); got != on {
			t.Errorf("quiet=%v: slim changed more than the four rules:\noff:\n%s\non:\n%s", quiet, off, on)
		}
	}
}

// An anchor run gets no chat turn note, so slim never touches its batch.
func TestSlimLeavesAnchorRunsWhole(t *testing.T) {
	off := buildNote(slimTestInput(false, false, false))
	on := buildNote(slimTestInput(false, false, true))
	if on != off {
		t.Fatalf("slim changed an anchor run note:\noff:\n%s\non:\n%s", off, on)
	}
	if !strings.Contains(on, routerBoundary) || strings.Contains(on, chatBatchRulesPointer) {
		t.Fatalf("anchor run note lacks the full boundary:\n%s", on)
	}
}

// M-206 and M-210 with slim on: the batch still says a quiet reply ends with
// every open item, and the turn note carries the reply format's "a quiet one
// included" rule the batch no longer repeats.
func TestSlimQuietChatBatchStillListsOpenItems(t *testing.T) {
	note := buildNote(slimTestInput(true, true, true))
	for _, want := range []string{
		"then end it with every open item as in any reply",
		"Quiet means no alert, never fewer open items",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("slim quiet chat batch lacks %q:\n%s", want, note)
		}
	}
	turnNote := buildChatTurnNote([]chatTurnScope{{SubscriptionID: "sub-1", Label: "issue tree RT-1", Enabled: true}})
	for _, want := range replyFormatRules {
		if !strings.Contains(turnNote, want) {
			t.Errorf("chat turn note lacks %q", want)
		}
	}
	for _, banned := range []string{"does not repeat", "gets no reply"} {
		if strings.Contains(note, banned) {
			t.Errorf("slim quiet chat batch says %q:\n%s", banned, note)
		}
	}
}
