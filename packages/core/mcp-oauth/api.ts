// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
//
// Connections to OAuth MCP entries. The entries themselves are ordinary MCP
// entries (`"auth": {"type": "oauth"}`) saved through the agent's mcp_config
// and the workspace MCP library; these endpoints only show and change
// connections:
//   /api/agents/{id}/mcp-oauth          the agent owner's, per entry name
//   /api/workspaces/{id}/mcp-oauth      the caller's own, per library entry
//   /api/mcp-oauth/complete             the end of any connect
// They answer 404 while `mcp_oauth_v1` is off.

import { api, ApiError, parseWithFallback } from "../api";
import {
  McpOAuthAgentListSchema,
  McpOAuthCompletionSchema,
  McpOAuthConnectSchema,
  McpOAuthWorkspaceListSchema,
} from "./schemas";
import type {
  CompleteMcpOAuthInput,
  McpOAuthAgentList,
  McpOAuthCompletion,
  McpOAuthWorkspaceList,
} from "./types";

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

function agentBase(agentId: string): string {
  return `/api/agents/${encodeURIComponent(agentId)}/mcp-oauth`;
}

function workspaceBase(wsId: string): string {
  return `/api/workspaces/${encodeURIComponent(wsId)}/mcp-oauth`;
}

/** The endpoint's body, or `unavailable` when it answers 404 (mcp_oauth_v1 off). */
async function requestOrUnavailable(
  path: string,
  init: RequestInit,
): Promise<{ unavailable: false; raw: unknown } | { unavailable: true }> {
  try {
    return { unavailable: false, raw: await api.request(path, init) };
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return { unavailable: true };
    throw err;
  }
}

async function startConnect(path: string, wsId: string, returnTo: string): Promise<string> {
  const raw = await api.request(path, scoped(wsId, { method: "POST", body: JSON.stringify({ return_to: returnTo }) }));
  const parsed = McpOAuthConnectSchema.safeParse(raw);
  if (!parsed.success) throw new Error("POST mcp-oauth connect: unexpected response");
  return parsed.data.authorization_url;
}

/** The OAuth entries an agent runs with and its owner's connection to each. */
export async function listAgentMcpOAuth(wsId: string, agentId: string): Promise<McpOAuthAgentList> {
  const result = await requestOrUnavailable(agentBase(agentId), scoped(wsId));
  if (result.unavailable) return { servers: [], can_connect: false, available: false };
  const parsed = parseWithFallback(result.raw, McpOAuthAgentListSchema, { servers: [], can_connect: false }, {
    endpoint: "GET /api/agents/{id}/mcp-oauth",
  });
  return { ...parsed, available: true };
}

/**
 * Starts connecting the agent owner to the entry under `name` and returns the
 * authorization server's URL to open. Only the agent's owner may.
 */
export function connectAgentMcpOAuth(wsId: string, agentId: string, name: string, returnTo: string): Promise<string> {
  return startConnect(`${agentBase(agentId)}/${encodeURIComponent(name)}/connect`, wsId, returnTo);
}

export async function disconnectAgentMcpOAuth(wsId: string, agentId: string, name: string): Promise<void> {
  await api.request(`${agentBase(agentId)}/${encodeURIComponent(name)}/connection`, scoped(wsId, { method: "DELETE" }));
}

/** The workspace library's OAuth entries and the caller's own connection to each. */
export async function listWorkspaceMcpOAuth(wsId: string): Promise<McpOAuthWorkspaceList> {
  const result = await requestOrUnavailable(workspaceBase(wsId), scoped(wsId));
  if (result.unavailable) return { servers: [], available: false };
  const parsed = parseWithFallback(result.raw, McpOAuthWorkspaceListSchema, { servers: [] }, {
    endpoint: "GET /api/workspaces/{id}/mcp-oauth",
  });
  return { ...parsed, available: true };
}

/** Starts connecting the caller to a workspace library entry. */
export function connectWorkspaceMcpOAuth(wsId: string, serverId: string, returnTo: string): Promise<string> {
  return startConnect(`${workspaceBase(wsId)}/${encodeURIComponent(serverId)}/connect`, wsId, returnTo);
}

export async function disconnectWorkspaceMcpOAuth(wsId: string, serverId: string): Promise<void> {
  await api.request(`${workspaceBase(wsId)}/${encodeURIComponent(serverId)}/connection`, scoped(wsId, { method: "DELETE" }));
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
