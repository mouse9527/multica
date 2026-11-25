// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  connectAgentMcpOAuth,
  connectWorkspaceMcpOAuth,
  disconnectAgentMcpOAuth,
  disconnectWorkspaceMcpOAuth,
  listAgentMcpOAuth,
  listWorkspaceMcpOAuth,
} from "./api";

export const mcpOAuthKeys = {
  all: (wsId: string) => ["mcp-oauth", wsId] as const,
  agent: (wsId: string, agentId: string) => [...mcpOAuthKeys.all(wsId), "agent", agentId] as const,
  workspace: (wsId: string) => [...mcpOAuthKeys.all(wsId), "workspace"] as const,
};

/**
 * `version` names what decides which entries use OAuth (the caller's digest
 * of the agent's config and bindings): a new one reads the list again.
 */
export function agentMcpOAuthOptions(wsId: string, agentId: string, enabled = true, version = "") {
  return queryOptions({
    queryKey: [...mcpOAuthKeys.agent(wsId, agentId), version] as const,
    queryFn: () => listAgentMcpOAuth(wsId, agentId),
    enabled: enabled && wsId !== "" && agentId !== "",
  });
}

export function workspaceMcpOAuthOptions(wsId: string, enabled = true, version = "") {
  return queryOptions({
    queryKey: [...mcpOAuthKeys.workspace(wsId), version] as const,
    queryFn: () => listWorkspaceMcpOAuth(wsId),
    enabled: enabled && wsId !== "",
  });
}

/**
 * A disconnect, or an entry saved or removed elsewhere, can change what both
 * the agent and the workspace lists show: one workspace connection is shared
 * by all of a user's agents. So every change refreshes every OAuth list of
 * the workspace.
 */
export function useInvalidateMcpOAuth(wsId: string) {
  const qc = useQueryClient();
  return () => qc.invalidateQueries({ queryKey: mcpOAuthKeys.all(wsId) });
}

export function useDisconnectAgentMcpOAuth(wsId: string, agentId: string) {
  const invalidate = useInvalidateMcpOAuth(wsId);
  return useMutation({
    mutationFn: (name: string) => disconnectAgentMcpOAuth(wsId, agentId, name),
    onSettled: invalidate,
  });
}

export function useDisconnectWorkspaceMcpOAuth(wsId: string) {
  const invalidate = useInvalidateMcpOAuth(wsId);
  return useMutation({
    mutationFn: (serverId: string) => disconnectWorkspaceMcpOAuth(wsId, serverId),
    onSettled: invalidate,
  });
}

/** Starts a connect; resolves with the URL to open. Changes nothing cached. */
export function useConnectAgentMcpOAuth(wsId: string, agentId: string) {
  return useMutation({
    mutationFn: ({ name, returnTo }: { name: string; returnTo: string }) =>
      connectAgentMcpOAuth(wsId, agentId, name, returnTo),
  });
}

export function useConnectWorkspaceMcpOAuth(wsId: string) {
  return useMutation({
    mutationFn: ({ serverId, returnTo }: { serverId: string; returnTo: string }) =>
      connectWorkspaceMcpOAuth(wsId, serverId, returnTo),
  });
}
