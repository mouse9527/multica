// Added by the mouse9527/multica fork: Router page settings (M-105).

import { queryOptions } from "@tanstack/react-query";
import { listRouterDispatches, listRouterSubscriptionChanges, listRouterSubscriptions } from "./api";

/** Router state changes without realtime events, so open views poll. */
export const ROUTER_POLL_MS = 30_000;

export const routerKeys = {
  all: (wsId: string) => ["router", wsId] as const,
  subscriptions: (wsId: string) => [...routerKeys.all(wsId), "subscriptions"] as const,
  dispatches: (wsId: string) => [...routerKeys.all(wsId), "dispatches"] as const,
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

export function routerDispatchesOptions(wsId: string) {
  return queryOptions({
    queryKey: routerKeys.dispatches(wsId),
    queryFn: () => listRouterDispatches(wsId),
    refetchInterval: ROUTER_POLL_MS,
  });
}

export function routerChangesOptions(wsId: string, subscriptionId: string) {
  return queryOptions({
    queryKey: routerKeys.changes(wsId, subscriptionId),
    queryFn: () => listRouterSubscriptionChanges(wsId, subscriptionId),
    enabled: Boolean(subscriptionId),
  });
}
