// Modified for the mouse9527/multica fork: Router notices in the Router chat (M-111).

package agentrouter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ChatPoster writes a Router system notice into a chat as an assistant
// message (service.PostRouterChatNotice). It returns the message id.
type ChatPoster interface {
	PostRouterChatNotice(ctx context.Context, chatSessionID, agentID, content string) (string, error)
}

// ChatAdapter delivers the dispatcher's own notices about a chat-bound
// subscription (a dispatch pause, a busy Router) into that subscription's
// Router chat, where the owner talks to the Router, so they get the chat
// reply alert rather than an inbox item they may never open. The inbox stays
// the fallback (see FallbackAdapter): it delivers whatever this adapter did
// not send, including every notice about a chat that can no longer be used.
//
// Notices an agent sends (multica notify owner) are left to the inbox: a
// chat-bound Router talks to its owner by replying in the chat itself.
type ChatAdapter struct {
	Store  *Store
	Poster ChatPoster
}

func (ChatAdapter) Channel() string { return "chat" }

func (a ChatAdapter) Deliver(ctx context.Context, n Notice) ChannelDelivery {
	if n.SenderType != "system" || n.SourceSubscriptionID == nil {
		return ChannelDelivery{State: DeliverySkipped, Reason: "not a Router system notice"}
	}
	chatID, agentID, err := usableChatOf(ctx, a.Store.DB, *n.SourceSubscriptionID, n.RecipientUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChannelDelivery{State: DeliverySkipped, Reason: "the subscription has no usable Router chat"}
	}
	if err != nil {
		return ChannelDelivery{State: DeliveryFailed, Reason: err.Error()}
	}
	id, err := a.Poster.PostRouterChatNotice(ctx, chatID, agentID, chatNoticeText(n))
	if err != nil {
		return ChannelDelivery{State: DeliveryFailed, Reason: err.Error()}
	}
	return ChannelDelivery{State: DeliverySent, Ref: id}
}

// usableChatOf returns the chat a subscription is bound to when that chat can
// still carry a notice to recipient: active, with the subscribed agent,
// started by recipient, who is still a member of the workspace. These are the
// conditions under which dispatch keeps sending batches there (see
// unusableChats).
func usableChatOf(ctx context.Context, q Querier, subscriptionID, recipient string) (chatID, agentID string, err error) {
	err = q.QueryRow(ctx, `
		SELECT cs.id::text, s.agent_id::text
		FROM router_chat_anchor ca
		JOIN router_subscription s ON s.id = ca.subscription_id
		JOIN chat_session cs ON cs.id = ca.chat_session_id
		WHERE ca.subscription_id = $1::uuid
		  AND cs.status = 'active' AND cs.agent_id = s.agent_id AND cs.creator_id = $2::uuid
		  AND EXISTS (SELECT 1 FROM member m WHERE m.workspace_id = cs.workspace_id AND m.user_id = cs.creator_id)`,
		subscriptionID, recipient).Scan(&chatID, &agentID)
	return chatID, agentID, err
}

// RouterNoticeLabel opens every notice written into a Router chat: the
// message shows as the agent's, but the system wrote it.
const RouterNoticeLabel = "[Router 系统通知 · 非 Router 回复]"

func chatNoticeText(n Notice) string {
	return RouterNoticeLabel + "\n\n**" + n.Title + "**\n\n" + n.Body
}
