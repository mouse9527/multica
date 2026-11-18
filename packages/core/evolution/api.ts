// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).
//
// Evolution endpoints (`/api/evolution/*`, see docs/evolution.md). They answer
// 404 while `evolution_v1` is off.

import { api, ApiError, parseWithFallback } from "../api";
import { EvolutionStatusSchema } from "./schemas";
import type { EnableEvolutionInput, EvolutionAgent, EvolutionStatus } from "./types";

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
