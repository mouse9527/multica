// Added by the mouse9527/multica fork: Router page settings (M-105).

import type { RouterDispatch, RouterInstance, RouterStatus, RouterSubscription } from "./types";

/**
 * The member's Routers: subscriptions grouped by the chat they are bound to,
 * kept only for chats in `ownChatIds` (the member's own chats). A binding
 * requires the chat's creator to be the subscription's owner, so these are
 * exactly the Routers that answer to the member. Subscriptions keep their
 * server order (oldest first).
 */
export function groupRouters(subs: RouterSubscription[], ownChatIds: ReadonlySet<string>): RouterInstance[] {
  const byChat = new Map<string, RouterInstance>();
  for (const sub of subs) {
    const chat = sub.anchor_chat_session_id;
    if (!chat || !ownChatIds.has(chat)) continue;
    const router = byChat.get(chat) ?? { chat_session_id: chat, agent_id: sub.agent_id, subscriptions: [] };
    router.subscriptions.push(sub);
    byChat.set(chat, router);
  }
  return [...byChat.values()];
}

/**
 * The member's subscriptions not bound to any chat, such as those kept when
 * a Router was released. A subscription without an owner answers to its
 * agent's owner (`agentOwners`: agent id → owner user id), as on the server.
 */
export function unboundSubscriptions(
  subs: RouterSubscription[],
  userId: string,
  agentOwners: ReadonlyMap<string, string | null>,
): RouterSubscription[] {
  return subs.filter(
    (s) => !s.anchor_chat_session_id && (s.owner_user_id ?? agentOwners.get(s.agent_id) ?? null) === userId,
  );
}

/** The subscriptions bound to one chat. */
export function routerSubscriptionsOfChat(subs: RouterSubscription[], chatSessionId: string): RouterSubscription[] {
  return subs.filter((s) => s.anchor_chat_session_id === chatSessionId);
}

/**
 * Paused when any subscription is: by hand (capture stopped, `manual`) or by
 * dispatch (`rate`, `agent_unavailable`; capture goes on). Reasons are
 * distinct, in subscription order.
 */
export function routerStatus(subs: RouterSubscription[]): RouterStatus {
  const reasons: string[] = [];
  for (const s of subs) {
    const reason = !s.enabled ? (s.paused_reason ?? "manual") : s.paused_reason;
    if (reason && !reasons.includes(reason)) reasons.push(reason);
  }
  return reasons.length ? { kind: "paused", reasons } : { kind: "running" };
}

/** Whether a subscription is paused for any reason. */
export function isSubscriptionPaused(sub: RouterSubscription): boolean {
  return !sub.enabled || sub.paused_reason !== null;
}

/** The newest dispatch of any of the subscriptions. */
export function latestDispatch(
  dispatches: RouterDispatch[],
  subs: RouterSubscription[],
): RouterDispatch | null {
  const ids = new Set(subs.map((s) => s.id));
  let latest: RouterDispatch | null = null;
  for (const d of dispatches) {
    if (!ids.has(d.subscription_id)) continue;
    if (!latest || d.created_at > latest.created_at) latest = d;
  }
  return latest;
}

/** The oldest reconcile bound among the subscriptions: how far capture is verified. */
export function reconciledThrough(subs: RouterSubscription[]): string | null {
  let oldest: string | null = null;
  for (const s of subs) {
    if (!s.reconciled_through) return null;
    if (!oldest || s.reconciled_through < oldest) oldest = s.reconciled_through;
  }
  return oldest;
}
