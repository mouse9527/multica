// Added by the mouse9527/multica fork: Router page settings (M-105), the
// chat list's Router marker (M-108), and live Super Agent settings (M-147).

import { queryOptions, useQueries } from "@tanstack/react-query";
import { listRouterChats, listRouterDispatches, listRouterSubscriptionChanges, listRouterSubscriptions } from "./api";
import { latestDispatch } from "./group";
import type { RouterDispatch, RouterSubscription } from "./types";

/**
 * Subscription writes through the API announce themselves
 * (router:subscription_changed, handled by the realtime sync), but dispatch
 * progress and automatic pauses do not, so open views also poll.
 */
export const ROUTER_POLL_MS = 30_000;

export const routerKeys = {
  all: (wsId: string) => ["router", wsId] as const,
  subscriptions: (wsId: string) => [...routerKeys.all(wsId), "subscriptions"] as const,
  chats: (wsId: string) => [...routerKeys.all(wsId), "chats"] as const,
  dispatches: (wsId: string) => [...routerKeys.all(wsId), "dispatches"] as const,
  lastDispatch: (wsId: string, subscriptionId: string) =>
    [...routerKeys.all(wsId), "dispatches", "last", subscriptionId] as const,
  changes: (wsId: string, subscriptionId: string) =>
    [...routerKeys.all(wsId), "changes", subscriptionId] as const,
};

/**
 * Always stale, so a settings view that opens reads the list again rather
 * than the copy an earlier view left in the cache, which may predate a change
 * made while the socket was down.
 */
export function routerSubscriptionsOptions(wsId: string) {
  return queryOptions({
    queryKey: routerKeys.subscriptions(wsId),
    queryFn: () => listRouterSubscriptions(wsId),
    staleTime: 0,
    refetchInterval: ROUTER_POLL_MS,
  });
}

/** The Routers among the member's chats, one request for the whole chat list. */
export function routerChatsOptions(wsId: string) {
  return queryOptions({
    queryKey: routerKeys.chats(wsId),
    queryFn: () => listRouterChats(wsId),
    refetchInterval: ROUTER_POLL_MS,
  });
}

/** A subscription's newest dispatch, read on its own so busy Routers elsewhere cannot crowd it out. */
export function routerLastDispatchOptions(wsId: string, subscriptionId: string) {
  return queryOptions({
    queryKey: routerKeys.lastDispatch(wsId, subscriptionId),
    queryFn: () => listRouterDispatches(wsId, { subscriptionId, limit: 1 }),
    refetchInterval: ROUTER_POLL_MS,
  });
}

/** The newest dispatch among a Router's subscriptions. */
export function useLastRouterDispatch(wsId: string, subs: RouterSubscription[]): RouterDispatch | null {
  const results = useQueries({ queries: subs.map((s) => routerLastDispatchOptions(wsId, s.id)) });
  return latestDispatch(
    results.flatMap((r) => r.data ?? []),
    subs,
  );
}

export function routerChangesOptions(wsId: string, subscriptionId: string) {
  return queryOptions({
    queryKey: routerKeys.changes(wsId, subscriptionId),
    queryFn: () => listRouterSubscriptionChanges(wsId, subscriptionId),
    enabled: Boolean(subscriptionId),
  });
}
