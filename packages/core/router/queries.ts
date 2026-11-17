// Added by the mouse9527/multica fork: Router page settings (M-105).

import { queryOptions, useQueries } from "@tanstack/react-query";
import { listRouterDispatches, listRouterSubscriptionChanges, listRouterSubscriptions } from "./api";
import { latestDispatch } from "./group";
import type { RouterDispatch, RouterSubscription } from "./types";

/** Router state changes without realtime events, so open views poll. */
export const ROUTER_POLL_MS = 30_000;

export const routerKeys = {
  all: (wsId: string) => ["router", wsId] as const,
  subscriptions: (wsId: string) => [...routerKeys.all(wsId), "subscriptions"] as const,
  dispatches: (wsId: string) => [...routerKeys.all(wsId), "dispatches"] as const,
  lastDispatch: (wsId: string, subscriptionId: string) =>
    [...routerKeys.all(wsId), "dispatches", "last", subscriptionId] as const,
  changes: (wsId: string, subscriptionId: string) =>
    [...routerKeys.all(wsId), "changes", subscriptionId] as const,
};

export function routerSubscriptionsOptions(wsId: string) {
  return queryOptions({
    queryKey: routerKeys.subscriptions(wsId),
    queryFn: () => listRouterSubscriptions(wsId),
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
