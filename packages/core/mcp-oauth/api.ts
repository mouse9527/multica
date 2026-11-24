// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
//
// Endpoints of an agent's OAuth MCP servers (`/api/agents/{id}/mcp-oauth`)
// and the end of a connect (`/api/mcp-oauth/complete`). They answer 404
// while `mcp_oauth_v1` is off.

import { api, ApiError, parseWithFallback } from "../api";
import {
  McpOAuthCompletionSchema,
  McpOAuthConnectSchema,
  McpOAuthServerListSchema,
  McpOAuthServerSchema,
} from "./schemas";
import type {
  CompleteMcpOAuthInput,
  CreateMcpOAuthServerInput,
  McpOAuthCompletion,
  McpOAuthServer,
  McpOAuthServerList,
} from "./types";

// Callers pass headers as a plain object; they are kept, and the workspace
// headers are added on top.
function scoped(wsId: string, init: RequestInit = {}): RequestInit {
  return {
    ...init,
    headers: {
      ...(init.headers as Record<string, string> | undefined),
      "X-Workspace-Slug": "",
      "X-Workspace-ID": wsId,
    },
  };
}

function base(agentId: string): string {
  return `/api/agents/${encodeURIComponent(agentId)}/mcp-oauth`;
}

function serverPath(agentId: string, serverId: string, suffix = ""): string {
  return `${base(agentId)}/${encodeURIComponent(serverId)}${suffix}`;
}

const UNAVAILABLE: McpOAuthServerList = { servers: [], can_connect: false, available: false };

export async function listMcpOAuthServers(wsId: string, agentId: string): Promise<McpOAuthServerList> {
  let raw: unknown;
  try {
    raw = await api.request(base(agentId), scoped(wsId));
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return UNAVAILABLE;
    throw err;
  }
  const parsed = parseWithFallback(raw, McpOAuthServerListSchema, { servers: [], can_connect: false }, {
    endpoint: "GET /api/agents/{id}/mcp-oauth",
  });
  return { ...parsed, available: true };
}

export async function createMcpOAuthServer(
  wsId: string,
  agentId: string,
  input: CreateMcpOAuthServerInput,
): Promise<McpOAuthServer> {
  const raw = await api.request(
    base(agentId),
    scoped(wsId, {
      method: "POST",
      body: JSON.stringify({
        name: input.name,
        url: input.url,
        scope: input.scope ?? "",
        extra_headers: input.extraHeaders ?? {},
      }),
    }),
  );
  const parsed = McpOAuthServerSchema.safeParse(raw);
  if (!parsed.success) throw new Error("POST /api/agents/{id}/mcp-oauth: unexpected response");
  return parsed.data;
}

export async function deleteMcpOAuthServer(wsId: string, agentId: string, serverId: string): Promise<void> {
  await api.request(serverPath(agentId, serverId), scoped(wsId, { method: "DELETE" }));
}

/**
 * Starts a connect and returns the authorization server's URL to open. Only
 * the agent's owner may; returnTo is the app path to come back to.
 */
export async function connectMcpOAuthServer(
  wsId: string,
  agentId: string,
  serverId: string,
  returnTo: string,
): Promise<string> {
  const raw = await api.request(
    serverPath(agentId, serverId, "/connect"),
    scoped(wsId, { method: "POST", body: JSON.stringify({ return_to: returnTo }) }),
  );
  const parsed = McpOAuthConnectSchema.safeParse(raw);
  if (!parsed.success) throw new Error("POST /api/agents/{id}/mcp-oauth/{serverId}/connect: unexpected response");
  return parsed.data.authorization_url;
}

export async function disconnectMcpOAuthServer(wsId: string, agentId: string, serverId: string): Promise<void> {
  await api.request(serverPath(agentId, serverId, "/connection"), scoped(wsId, { method: "DELETE" }));
}

/**
 * Finishes a connect with the signed-in user's session: the server accepts
 * it only from the user who started it, and only once.
 */
export async function completeMcpOAuth(input: CompleteMcpOAuthInput): Promise<McpOAuthCompletion> {
  const raw = await api.request("/api/mcp-oauth/complete", {
    method: "POST",
    body: JSON.stringify({ state: input.state, code: input.code, error: input.error ?? "" }),
  });
  const parsed = McpOAuthCompletionSchema.safeParse(raw);
  if (!parsed.success) throw new Error("POST /api/mcp-oauth/complete: unexpected response");
  return parsed.data;
}
