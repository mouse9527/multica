// Added by the mouse9527/multica fork: the Super Agent note on every chat turn (M-175), the create warning (M-194), the reply format (M-206), and wakeups on issues in scope (M-208).

package agentrouter

import (
	"context"
	"fmt"
	"strings"
)

// routerSubscribedMeans: an agent that creates an issue is its `creator`
// subscriber at once, and the upstream chat prompt asks for a chat wakeup on
// handed-off work, so a Super Agent told the owner it "subscribed" to issues
// no batch would ever report (M-175). The rule and its examples are in
// `multica router guide`.
const routerSubscribedMeans = "In this chat, \"subscribed\" (订阅) means only a scope of this Super Agent: " +
	"an issue you create is in scope only under a subscribed issue tree or in a subscribed project " +
	"(`multica issue create` prints a \"Super Agent:\" warning when the new issue is not; " +
	"check others with `multica router scope <subscription-id>`). " +
	"Issue subscribers (the automatic `creator` one included) and chat wakeups are not subscriptions. " +
	"When it is out of scope, say so and ask the owner whether to add it " +
	"(`multica router subscribe --issue <id> --anchor-chat current`, only after they agree). " +
	"See \"自己新建的 issue 在不在范围里\" in `multica router guide`.\n"

// chatTurnScope is one subscription bound to a chat, as the turn note names it.
type chatTurnScope struct {
	SubscriptionID string
	Label          string
	Enabled        bool
	PausedReason   *string
}

// ChatTurnNote is what every turn in a Super Agent chat is told about it,
// owner turns included: batch turns carry the boundary in the batch itself,
// but a turn the owner starts would otherwise not know the chat is a Super
// Agent at all. The turns a chat wakeup starts get it too, which is where the
// wakeup rules have to reach. It is "" for a chat no subscription is bound to.
func (s *Store) ChatTurnNote(ctx context.Context, workspaceID, chatSessionID string) (string, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT s.id::text, s.enabled, s.paused_reason,
		       CASE WHEN s.scope_type = 'issue_tree' THEN 'issue tree ' || COALESCE(
		                (SELECT w.issue_prefix || '-' || i.number FROM issue i JOIN workspace w ON w.id = i.workspace_id
		                 WHERE i.id = s.scope_id AND i.workspace_id = s.workspace_id), s.scope_id::text)
		            ELSE 'project ' || COALESCE(
		                (SELECT p.title FROM project p WHERE p.id = s.scope_id AND p.workspace_id = s.workspace_id), s.scope_id::text)
		       END
		FROM router_chat_anchor ca
		JOIN router_subscription s ON s.id = ca.subscription_id AND s.workspace_id = $1::uuid
		WHERE ca.workspace_id = $1::uuid AND ca.chat_session_id = $2::uuid
		ORDER BY s.created_at, s.id`, workspaceID, chatSessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var scopes []chatTurnScope
	for rows.Next() {
		var sc chatTurnScope
		if err := rows.Scan(&sc.SubscriptionID, &sc.Enabled, &sc.PausedReason, &sc.Label); err != nil {
			return "", err
		}
		scopes = append(scopes, sc)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return buildChatTurnNote(scopes), nil
}

func buildChatTurnNote(scopes []chatTurnScope) string {
	if len(scopes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Super Agent\n\n")
	b.WriteString("This chat is a Super Agent (technical name: router). Batches report events on the issues in its scopes:\n")
	for _, sc := range scopes {
		state := "running"
		switch {
		case sc.PausedReason != nil:
			state = "paused: " + *sc.PausedReason
		case !sc.Enabled:
			state = "paused"
		}
		fmt.Fprintf(&b, "- %s (subscription %s, %s)\n", sc.Label, sc.SubscriptionID, state)
	}
	b.WriteString(routerSubscribedMeans)
	b.WriteString(routerWakeupSplit)
	b.WriteString(routerOwnerDecisions)
	b.WriteString(routerReplyFormat)
	b.WriteString("What a Super Agent does itself and what it hands off, and the commands for its settings, are in `multica router guide`.")
	return b.String()
}
