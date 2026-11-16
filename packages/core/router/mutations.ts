// Added by the mouse9527/multica fork: Router page settings (M-105).

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useWorkspaceId } from "../hooks";
import type { Issue } from "../types";
import {
  createRouterSubscription,
  deleteRouterSubscription,
  setRouterSubscriptionPaused,
  updateRouterSubscription,
} from "./api";
import { isSubscriptionPaused } from "./group";
import { routerKeys } from "./queries";
import type { RouterScopeInput, RouterSettingsInput, RouterSubscription } from "./types";

export interface AddRouterScopesInput {
  chatSessionId: string;
  agentId: string;
  scopes: RouterScopeInput[];
  settings: RouterSettingsInput;
  /**
   * A project scope has no issue of its own for dispatch records to name, so
   * a board issue is created in that project as its anchor. The private
   * project fence checks the anchor's project, so each project gets its own.
   */
  board: { title: string; description: string };
  /**
   * The workspace's subscriptions as last read. An agent can subscribe to a
   * scope only once, so a scope it already has is taken over instead of
   * created: an unbound one (kept when a Router was released) is bound to
   * this chat and resumed; one bound to another chat is refused.
   */
  existing: RouterSubscription[];
}

export interface AddRouterScopesResult {
  created: RouterSubscription[];
  boards: Issue[];
  failed: { scope: RouterScopeInput; error: unknown }[];
}

/** A scope the agent already covers in another chat's Router. */
export class RouterScopeTakenError extends Error {
  constructor(readonly chatSessionId: string) {
    super(`this scope already belongs to the Router of chat ${chatSessionId}`);
    this.name = "RouterScopeTakenError";
  }
}

/** The agent's subscription to this scope, if it has one. */
export function findScopeSubscription(
  subs: RouterSubscription[],
  agentId: string,
  scope: RouterScopeInput,
): RouterSubscription | undefined {
  return subs.find((s) => s.agent_id === agentId && s.scope_type === scope.scope_type && s.scope_id === scope.scope_id);
}

/**
 * Binds a kept subscription to the chat with the form's settings, then
 * resumes it. Bound while still paused, it never runs unbound in between.
 */
async function takeOverScope(
  wsId: string,
  sub: RouterSubscription,
  input: AddRouterScopesInput,
): Promise<RouterSubscription> {
  let out = await updateRouterSubscription(wsId, sub.id, {
    ...input.settings,
    anchor_chat_session_id: input.chatSessionId,
  });
  if (isSubscriptionPaused(out)) out = await setRouterSubscriptionPaused(wsId, sub.id, false);
  return out;
}

/**
 * Adds each scope to the chat's Router: a new subscription bound to the chat
 * in the same request, or the agent's existing unbound one taken over. A
 * scope that fails does not stop the others; a board issue made for it is
 * deleted again.
 */
export async function addRouterScopes(wsId: string, input: AddRouterScopesInput): Promise<AddRouterScopesResult> {
  const result: AddRouterScopesResult = { created: [], boards: [], failed: [] };
  for (const scope of input.scopes) {
    let board: Issue | null = null;
    try {
      const existing = findScopeSubscription(input.existing, input.agentId, scope);
      if (existing) {
        const bound = existing.anchor_chat_session_id;
        if (bound && bound !== input.chatSessionId) throw new RouterScopeTakenError(bound);
        result.created.push(await takeOverScope(wsId, existing, input));
        continue;
      }
      if (scope.scope_type === "project") {
        board = await api.createIssue({
          title: input.board.title,
          description: input.board.description,
          project_id: scope.scope_id,
        });
      }
      const sub = await createRouterSubscription(wsId, {
        ...scope,
        ...input.settings,
        agent_id: input.agentId,
        anchor_chat_session_id: input.chatSessionId,
        ...(board ? { anchor_issue_id: board.id } : {}),
      });
      result.created.push(sub);
      if (board) result.boards.push(board);
    } catch (error) {
      if (board) await api.deleteIssue(board.id).catch(() => undefined);
      result.failed.push({ scope, error });
    }
  }
  return result;
}

/** Pausing stops capture; resuming also clears an automatic dispatch pause. */
export async function setRouterPaused(
  wsId: string,
  subs: RouterSubscription[],
  paused: boolean,
): Promise<RouterSubscription[]> {
  const targets = paused ? subs.filter((s) => s.enabled) : subs.filter(isSubscriptionPaused);
  return Promise.all(targets.map((s) => setRouterSubscriptionPaused(wsId, s.id, paused)));
}

export async function updateRouterSettings(
  wsId: string,
  subs: RouterSubscription[],
  settings: RouterSettingsInput,
): Promise<RouterSubscription[]> {
  return Promise.all(subs.map((s) => updateRouterSubscription(wsId, s.id, settings)));
}

export type RouterReleaseMode = "delete" | "keep_paused";

/**
 * Stops the chat being a Router. `delete` removes the subscriptions;
 * `keep_paused` pauses them first and then unbinds them, so none of them
 * falls back to anchor runs in between.
 */
export async function releaseRouter(
  wsId: string,
  subs: RouterSubscription[],
  mode: RouterReleaseMode,
): Promise<void> {
  for (const s of subs) {
    if (mode === "delete") {
      await deleteRouterSubscription(wsId, s.id);
      continue;
    }
    if (s.enabled) await setRouterSubscriptionPaused(wsId, s.id, true);
    await updateRouterSubscription(wsId, s.id, { anchor_chat_session_id: "none" });
  }
}

function useRouterMutation<TInput, TResult>(fn: (wsId: string, input: TInput) => Promise<TResult>) {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (input: TInput) => fn(wsId, input),
    onSettled: () => qc.invalidateQueries({ queryKey: routerKeys.all(wsId) }),
  });
}

export function useAddRouterScopes() {
  return useRouterMutation(addRouterScopes);
}

export function useSetRouterPaused() {
  return useRouterMutation((wsId, input: { subs: RouterSubscription[]; paused: boolean }) =>
    setRouterPaused(wsId, input.subs, input.paused),
  );
}

export function useUpdateRouterSettings() {
  return useRouterMutation((wsId, input: { subs: RouterSubscription[]; settings: RouterSettingsInput }) =>
    updateRouterSettings(wsId, input.subs, input.settings),
  );
}

export function useRemoveRouterScope() {
  return useRouterMutation((wsId, sub: RouterSubscription) => deleteRouterSubscription(wsId, sub.id));
}

export function useReleaseRouter() {
  return useRouterMutation((wsId, input: { subs: RouterSubscription[]; mode: RouterReleaseMode }) =>
    releaseRouter(wsId, input.subs, input.mode),
  );
}
