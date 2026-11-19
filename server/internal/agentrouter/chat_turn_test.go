// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175).

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
