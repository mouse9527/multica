// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136, M-138).
//
// Evolution endpoints (`/api/evolution/*`, see docs/evolution.md). They answer
// 404 while `evolution_v1` is off.

import { api, ApiError, parseWithFallback } from "../api";
import { EvolutionConfigStateSchema, EvolutionProposalStateSchema, EvolutionStatusSchema } from "./schemas";
import type {
  EnableEvolutionInput,
  EvolutionAgent,
  EvolutionConfig,
  EvolutionConfigState,
  EvolutionProposalState,
  EvolutionStatus,
  SaveEvolutionConfigInput,
} from "./types";

function scoped(wsId: string, init: RequestInit = {}): RequestInit {
  return { ...init, headers: { "X-Workspace-Slug": "", "X-Workspace-ID": wsId } };
}

export async function getEvolution(wsId: string): Promise<EvolutionStatus> {
  let raw: unknown;
  try {
    raw = await api.request("/api/evolution", scoped(wsId));
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return { available: false, agent: null };
    throw err;
  }
  const parsed = parseWithFallback(raw, EvolutionStatusSchema, { agent: null }, {
    endpoint: "GET /api/evolution",
  });
  return { available: true, agent: parsed.agent };
}

export async function enableEvolution(wsId: string, input: EnableEvolutionInput): Promise<EvolutionAgent> {
  const raw = await api.request(
    "/api/evolution/enable",
    scoped(wsId, {
      method: "POST",
      body: JSON.stringify({
        runtime_id: input.runtimeId,
        language: input.language,
        model: input.model ?? "",
      }),
    }),
  );
  const parsed = EvolutionStatusSchema.safeParse(raw);
  if (!parsed.success || !parsed.data.agent) throw new Error("POST /api/evolution/enable: unexpected response");
  return parsed.data.agent;
}

const CONFIG_FALLBACK: EvolutionConfigState = EvolutionConfigStateSchema.parse({});

export async function getEvolutionConfig(wsId: string): Promise<EvolutionConfigState> {
  const raw = await api.request("/api/evolution/config", scoped(wsId));
  return parseWithFallback(raw, EvolutionConfigStateSchema, CONFIG_FALLBACK, {
    endpoint: "GET /api/evolution/config",
  });
}

/**
 * Replaces the configuration. The server answers 409 when the change would
 * start reading chats without `acknowledgeMemberNotice`.
 */
export async function saveEvolutionConfig(
  wsId: string,
  input: SaveEvolutionConfigInput,
): Promise<EvolutionConfigState> {
  const raw = await api.request(
    "/api/evolution/config",
    scoped(wsId, {
      method: "PUT",
      body: JSON.stringify({
        config: input.config,
        acknowledge_member_notice: input.acknowledgeMemberNotice ?? false,
      }),
    }),
  );
  const parsed = EvolutionConfigStateSchema.safeParse(raw);
  if (!parsed.success) throw new Error("PUT /api/evolution/config: unexpected response");
  return parsed.data;
}

/**
 * Whether saving `next` over `current` turns chat reading on, which notifies
 * every member and so needs their confirmation first.
 */
export function startsChatReading(current: EvolutionConfigState, next: EvolutionConfig): boolean {
  return next.enabled && next.sources.chat && !current.chat_active;
}

function proposalPath(issueId: string, action?: "apply" | "rollback"): string {
  const base = `/api/evolution/proposals/${encodeURIComponent(issueId)}`;
  return action ? `${base}/${action}` : base;
}

function parseProposalState(raw: unknown, endpoint: string): EvolutionProposalState {
  const parsed = EvolutionProposalStateSchema.safeParse(raw);
  if (!parsed.success) throw new Error(`${endpoint}: unexpected response`);
  return parsed.data;
}

/**
 * A proposal's apply and rollback state, or null when the issue is not a
 * proposal that can be applied (404, also while `evolution_v1` is off).
 */
export async function getEvolutionProposal(wsId: string, issueId: string): Promise<EvolutionProposalState | null> {
  let raw: unknown;
  try {
    raw = await api.request(proposalPath(issueId), scoped(wsId));
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null;
    throw err;
  }
  return parseWithFallback<EvolutionProposalState | null>(raw, EvolutionProposalStateSchema, null, {
    endpoint: "GET /api/evolution/proposals/{id}",
  });
}

/**
 * Applies a proposal. The server answers 409 when the target changed since
 * the proposal was written (and marks it stale) or it is already applied,
 * and 422 when the change cannot be written; the error carries its reason.
 */
export async function applyEvolutionProposal(wsId: string, issueId: string): Promise<EvolutionProposalState> {
  const raw = await api.request(proposalPath(issueId, "apply"), scoped(wsId, { method: "POST" }));
  return parseProposalState(raw, "POST /api/evolution/proposals/{id}/apply");
}

/** Rolls an applied proposal back; 409 when the target changed after the apply. */
export async function rollbackEvolutionProposal(wsId: string, issueId: string): Promise<EvolutionProposalState> {
  const raw = await api.request(proposalPath(issueId, "rollback"), scoped(wsId, { method: "POST" }));
  return parseProposalState(raw, "POST /api/evolution/proposals/{id}/rollback");
}
