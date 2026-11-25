"use client";

// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
//
// The OAuth state of the rows of the two MCP pages. The pages read only the
// flag (useMcpOAuthAvailable) and render these components into their rows
// while it is on, so with mcp_oauth_v1 off they do exactly what upstream does:
//   - an agent's MCP tab: the agent owner's connection to each of the agent's
//     own OAuth entries and of its bound workspace OAuth entries;
//   - the workspace MCP settings: the viewer's own connection to each OAuth
//     library entry.

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useFeatureEnabled } from "@multica/core/config";
import {
  MCP_OAUTH_FLAG,
  agentMcpOAuthOptions,
  mcpOAuthConnectionState,
  useConnectAgentMcpOAuth,
  useConnectWorkspaceMcpOAuth,
  useDisconnectAgentMcpOAuth,
  useDisconnectWorkspaceMcpOAuth,
  workspaceMcpOAuthOptions,
} from "@multica/core/mcp-oauth";
import type { Agent } from "@multica/core/types";
import { agentMcpServersOptions, workspaceMcpServersOptions } from "@multica/core/workspace/queries";
import { useT } from "../i18n";
import { McpOAuthConnection, useMcpOAuthActions } from "./mcp-oauth-connection";

/** Whether OAuth MCP is on (mcp_oauth_v1). */
export function useMcpOAuthAvailable(): boolean {
  return useFeatureEnabled(MCP_OAUTH_FLAG, false);
}

/**
 * A short digest of what decides which entries use OAuth. It goes into the
 * list's query key, so the list is read again when an entry is saved, bound
 * or removed — without putting the config itself, which can hold secrets,
 * into the cache key.
 */
function digest(value: unknown): string {
  const text = JSON.stringify(value ?? null);
  let hash = 5381;
  for (let i = 0; i < text.length; i++) hash = ((hash << 5) + hash + text.charCodeAt(i)) | 0;
  return (hash >>> 0).toString(36);
}

/**
 * The agent's OAuth entries and its owner's connections. The endpoint admits
 * the agent's owner and workspace owners/admins (`canEdit`).
 */
function useAgentMcpOAuthList(agent: Agent, canEdit: boolean) {
  const assigned = useQuery({ ...agentMcpServersOptions(agent.id), enabled: canEdit && agent.id !== "" });
  const version = useMemo(
    () =>
      digest([
        agent.mcp_config,
        (assigned.data ?? []).map((server) => [server.id, server.enabled !== false, server.updated_at]),
      ]),
    [agent.mcp_config, assigned.data],
  );
  return useQuery(agentMcpOAuthOptions(agent.workspace_id ?? "", agent.id, canEdit, version));
}

/**
 * The connection of one OAuth entry on an agent's MCP tab: its own entry by
 * `name`, or a bound workspace entry by `serverId`. Only the agent's owner
 * connects and disconnects, since runs use the owner's connection; a
 * workspace entry's connection is the owner's one, shared by all their
 * agents, which the row says.
 */
export function AgentMcpOAuthConnection({
  agent,
  canEdit,
  name,
  serverId,
}: {
  agent: Agent;
  canEdit: boolean;
  name?: string;
  serverId?: string;
}) {
  const { t } = useT("mcpoauth");
  const wsId = agent.workspace_id ?? "";
  const list = useAgentMcpOAuthList(agent, canEdit);
  const connect = useConnectAgentMcpOAuth(wsId, agent.id);
  const disconnect = useDisconnectAgentMcpOAuth(wsId, agent.id);
  const actions = useMcpOAuthActions();
  if (!list.data?.available) return null;
  const entry = list.data.servers.find((server) =>
    serverId !== undefined
      ? server.source === "workspace" && server.workspace_server_id === serverId
      : server.source !== "workspace" && server.name === name,
  );
  if (!entry) return null;
  const canConnect = list.data.can_connect;
  return (
    <McpOAuthConnection
      name={entry.name}
      note={entry.shared ? t(($) => $.shared_agent) : undefined}
      state={mcpOAuthConnectionState(entry.connection)}
      problem={entry.problem}
      canConnect={canConnect}
      canDisconnect={canConnect}
      pending={actions.pending !== null}
      onConnect={() => actions.connect("entry", (returnTo) => connect.mutateAsync({ name: entry.name, returnTo }))}
      onDisconnect={() => actions.disconnect("entry", entry.name, () => disconnect.mutateAsync(entry.name))}
    />
  );
}

/** Says so when the viewer is not the agent's owner and an entry still needs connecting. */
export function AgentMcpOAuthOwnerNote({ agent, canEdit }: { agent: Agent; canEdit: boolean }) {
  const { t } = useT("mcpoauth");
  const list = useAgentMcpOAuthList(agent, canEdit);
  const data = list.data;
  if (!data?.available || data.can_connect) return null;
  const waiting = data.servers.some(
    (entry) => !entry.problem && mcpOAuthConnectionState(entry.connection) !== "connected",
  );
  return waiting ? <p className="text-caption text-muted-foreground">{t(($) => $.owner_only)}</p> : null;
}

/**
 * The viewer's own connection to one workspace library entry. Any member
 * connects for themselves; the connection is then used by all of their agents
 * the entry is bound to.
 */
export function WorkspaceMcpOAuthConnection({ wsId, serverId }: { wsId: string; serverId: string }) {
  const { t } = useT("mcpoauth");
  const library = useQuery(workspaceMcpServersOptions(wsId));
  const version = useMemo(
    () => digest((library.data ?? []).map((server) => [server.id, server.updated_at])),
    [library.data],
  );
  const list = useQuery(workspaceMcpOAuthOptions(wsId, true, version));
  const connect = useConnectWorkspaceMcpOAuth(wsId);
  const disconnect = useDisconnectWorkspaceMcpOAuth(wsId);
  const actions = useMcpOAuthActions();
  if (!list.data?.available) return null;
  const entry = list.data.servers.find((server) => server.server_id === serverId);
  if (!entry) return null;
  return (
    <McpOAuthConnection
      name={entry.name}
      note={t(($) => $.shared_workspace)}
      state={mcpOAuthConnectionState(entry.connection)}
      problem={entry.problem}
      canConnect
      canDisconnect
      pending={actions.pending !== null}
      onConnect={() =>
        actions.connect("entry", (returnTo) => connect.mutateAsync({ serverId: entry.server_id, returnTo }))
      }
      onDisconnect={() => actions.disconnect("entry", entry.name, () => disconnect.mutateAsync(entry.server_id))}
    />
  );
}
