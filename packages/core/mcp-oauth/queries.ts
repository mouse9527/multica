// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  connectMcpOAuthServer,
  createMcpOAuthServer,
  deleteMcpOAuthServer,
  disconnectMcpOAuthServer,
  listMcpOAuthServers,
} from "./api";
import type { CreateMcpOAuthServerInput } from "./types";

export const mcpOAuthKeys = {
  all: (wsId: string) => ["mcp-oauth", wsId] as const,
  agent: (wsId: string, agentId: string) => [...mcpOAuthKeys.all(wsId), "agent", agentId] as const,
};

export function mcpOAuthServersOptions(wsId: string, agentId: string) {
  return queryOptions({
    queryKey: mcpOAuthKeys.agent(wsId, agentId),
    queryFn: () => listMcpOAuthServers(wsId, agentId),
    enabled: wsId !== "" && agentId !== "",
  });
}

function useAgentMcpOAuthMutation<TVariables, TResult>(
  wsId: string,
  agentId: string,
  mutationFn: (variables: TVariables) => Promise<TResult>,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn,
    onSettled: () => qc.invalidateQueries({ queryKey: mcpOAuthKeys.agent(wsId, agentId) }),
  });
}

export function useCreateMcpOAuthServer(wsId: string, agentId: string) {
  return useAgentMcpOAuthMutation(wsId, agentId, (input: CreateMcpOAuthServerInput) =>
    createMcpOAuthServer(wsId, agentId, input),
  );
}

export function useDeleteMcpOAuthServer(wsId: string, agentId: string) {
  return useAgentMcpOAuthMutation(wsId, agentId, (serverId: string) => deleteMcpOAuthServer(wsId, agentId, serverId));
}

export function useDisconnectMcpOAuthServer(wsId: string, agentId: string) {
  return useAgentMcpOAuthMutation(wsId, agentId, (serverId: string) =>
    disconnectMcpOAuthServer(wsId, agentId, serverId),
  );
}

/** Starts a connect; resolves with the URL to open. Changes nothing cached. */
export function useConnectMcpOAuthServer(wsId: string, agentId: string) {
  return useMutation({
    mutationFn: ({ serverId, returnTo }: { serverId: string; returnTo: string }) =>
      connectMcpOAuthServer(wsId, agentId, serverId, returnTo),
  });
}
