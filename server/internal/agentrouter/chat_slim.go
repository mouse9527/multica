// Added by the mouse9527/multica fork: chat batches without the rules every chat turn already carries (M-240).

package agentrouter

// Every turn in a Super Agent chat, batch turns included, gets the
// "## Super Agent" note (chat_turn.go): what "subscribed" means, the wakeup
// split, owner decisions and the reply format. A chat batch used to carry the
// same four rules again, and batches stay in the chat history, so each one
// grew the conversation by that much for nothing (M-234). With FlagBatchSlim
// on, a chat batch keeps only what the turn note lacks: its opening, the
// reply rule, the boundary, and a line pointing at the turn note. Anchor runs
// get no turn note, so their batches keep every rule.

const (
	// FlagBatchSlim is the feature flag (FF_ROUTER_BATCH_SLIM). It only takes
	// effect with router_chat on. Like FlagChatQuiet it is declared here so
	// the upstream flag file stays untouched.
	FlagBatchSlim = "router_batch_slim"

	// chatBatchRulesPointer replaces the four rules in a slim chat batch.
	chatBatchRulesPointer = "The rules on what \"subscribed\" means, chat wakeups, owner decisions and the reply format " +
		"are in this chat's Super Agent note (the \"## Super Agent\" section of your instructions) and in `multica router guide`; " +
		"they apply to this batch too.\n"
)

// chatBatchBoundary is the boundary a chat batch carries: all of it, or with
// slim on, the part the turn note lacks and a pointer to the rest.
func chatBatchBoundary(slim bool) string {
	if !slim {
		return routerBoundary
	}
	return routerBoundaryCore + chatBatchRulesPointer
}
