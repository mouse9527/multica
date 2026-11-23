// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175), the reply format (M-206), and wakeups on issues in scope (M-208).

package agentrouter

import (
	"strings"
	"testing"
)

func TestChatTurnNoteNamesScopesAndWhatSubscribedMeans(t *testing.T) {
	if got := buildChatTurnNote(nil); got != "" {
		t.Fatalf("no bound subscription: note = %q, want none", got)
	}
	rate := "rate"
	note := buildChatTurnNote([]chatTurnScope{
		{SubscriptionID: "sub-1", Label: "issue tree M-45", Enabled: true},
		{SubscriptionID: "sub-2", Label: "project multica", Enabled: false, PausedReason: &rate},
		{SubscriptionID: "sub-3", Label: "issue tree M-90", Enabled: false},
	})
	for _, want := range []string{
		"## Super Agent",
		"This chat is a Super Agent (technical name: router)",
		"- issue tree M-45 (subscription sub-1, running)",
		"- project multica (subscription sub-2, paused: rate)",
		"- issue tree M-90 (subscription sub-3, paused)",
		"\"subscribed\" (订阅) means only a scope of this Super Agent",
		"in scope only under a subscribed issue tree or in a subscribed project",
		"`multica router scope <subscription-id>`",
		"`multica issue create` prints a \"Super Agent:\" warning",
		"the automatic `creator` one",
		"chat wakeups are not subscriptions",
		"ask the owner whether to add it",
		"only after they agree",
		"`multica router subscribe --issue <id> --anchor-chat current`",
		"\"自己新建的 issue 在不在范围里\" in `multica router guide`",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
}

// A turn the owner starts answers the owner directly, so it gets the reply
// format and the owner-decision rule the batches carry (M-206).
func TestChatTurnNoteStatesReplyFormat(t *testing.T) {
	note := buildChatTurnNote([]chatTurnScope{{SubscriptionID: "sub-1", Label: "issue tree M-45", Enabled: true}})
	for _, want := range append([]string{"Every decision you ask of the owner must stand on its own"}, replyFormatRules...) {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	// A chat turn replies in the chat; pointing it at notify would send the
	// owner a second copy through another channel.
	if strings.Contains(note, "multica notify owner") {
		t.Errorf("chat turn note names `multica notify owner`:\n%s", note)
	}
}

// A chat wakeup starts a turn in the chat, so this note is where the wakeup
// rules reach it: say what kind a wakeup on an issue in scope is, and read
// current state when it fires (M-208).
func TestChatTurnNoteStatesWakeupRules(t *testing.T) {
	note := buildChatTurnNote([]chatTurnScope{{SubscriptionID: "sub-1", Label: "issue tree M-45", Enabled: true}})
	for _, want := range []string{
		"do not create a chat wakeup (`--until-*` condition or `--every` check) for an issue in scope",
		"say in your reply what kind it is",
		"a timed retry, not a repeat of the issue's events",
		"In a turn a wakeup starts, read the issue's current status and latest comments before reporting",
		"\"chat wakeup 和事件批次的分工\" in `multica router guide`",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
}
