// Added by the mouse9527/multica fork: Router page settings (M-105).
//
// Router endpoints (`/api/router/*`, see docs/router.md). They answer 404
// while `router_v1` is off; the UI only calls them when `router_chat` is on.

import { api, parseWithFallback } from "../api";
import {
  RouterDispatchListSchema,
  RouterSubscriptionChangeListSchema,
  RouterSubscriptionListSchema,
  RouterSubscriptionSchema,
} from "./schemas";
import type {
  RouterDispatch,
  RouterScopeInput,
  RouterSettingsInput,
  RouterSubscription,
  RouterSubscriptionChange,
} from "./types";

function scoped(wsId: string, init: RequestInit = {}): RequestInit {
  return { ...init, headers: { "X-Workspace-Slug": "", "X-Workspace-ID": wsId } };
}

/** A write's body must be a subscription; anything else fails the mutation. */
function parseWrite(raw: unknown, endpoint: string): RouterSubscription {
  const parsed = RouterSubscriptionSchema.safeParse(raw);
  if (!parsed.success) throw new Error(`${endpoint}: unexpected response`);
  return parsed.data;
}

export async function listRouterSubscriptions(wsId: string): Promise<RouterSubscription[]> {
  const raw = await api.forkRequest("/api/router/subscriptions", scoped(wsId));
  return parseWithFallback(raw, RouterSubscriptionListSchema, { subscriptions: [] as RouterSubscription[] }, {
    endpoint: "GET /api/router/subscriptions",
  }).subscriptions;
}

export async function listRouterDispatches(
  wsId: string,
  { subscriptionId, limit = 200 }: { subscriptionId?: string; limit?: number } = {},
): Promise<RouterDispatch[]> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (subscriptionId) params.set("subscription_id", subscriptionId);
  const raw = await api.forkRequest(`/api/router/dispatches?${params}`, scoped(wsId));
  return parseWithFallback(raw, RouterDispatchListSchema, { dispatches: [] as RouterDispatch[] }, {
    endpoint: "GET /api/router/dispatches",
  }).dispatches;
}

export async function listRouterSubscriptionChanges(
  wsId: string,
  subscriptionId: string,
): Promise<RouterSubscriptionChange[]> {
  const raw = await api.forkRequest(
    `/api/router/subscriptions/${encodeURIComponent(subscriptionId)}/changes`,
    scoped(wsId),
  );
  return parseWithFallback(raw, RouterSubscriptionChangeListSchema, { changes: [] as RouterSubscriptionChange[] }, {
    endpoint: "GET /api/router/subscriptions/:id/changes",
  }).changes;
}

export async function createRouterSubscription(
  wsId: string,
  input: RouterScopeInput &
    RouterSettingsInput & { agent_id: string; anchor_chat_session_id: string; anchor_issue_id?: string },
): Promise<RouterSubscription> {
  const raw = await api.forkRequest(
    "/api/router/subscriptions",
    scoped(wsId, { method: "POST", body: JSON.stringify(input) }),
  );
  return parseWrite(raw, "POST /api/router/subscriptions");
}

export async function updateRouterSubscription(
  wsId: string,
  id: string,
  patch: Partial<RouterSettingsInput> & { anchor_chat_session_id?: string },
): Promise<RouterSubscription> {
  const raw = await api.forkRequest(
    `/api/router/subscriptions/${encodeURIComponent(id)}`,
    scoped(wsId, { method: "PATCH", body: JSON.stringify(patch) }),
  );
  return parseWrite(raw, "PATCH /api/router/subscriptions/:id");
}

export async function setRouterSubscriptionPaused(
  wsId: string,
  id: string,
  paused: boolean,
): Promise<RouterSubscription> {
  const action = paused ? "pause" : "resume";
  const raw = await api.forkRequest(
    `/api/router/subscriptions/${encodeURIComponent(id)}/${action}`,
    scoped(wsId, { method: "POST" }),
  );
  return parseWrite(raw, `POST /api/router/subscriptions/:id/${action}`);
}

export async function deleteRouterSubscription(wsId: string, id: string): Promise<void> {
  await api.forkRequest(
    `/api/router/subscriptions/${encodeURIComponent(id)}`,
    scoped(wsId, { method: "DELETE" }),
  );
}
