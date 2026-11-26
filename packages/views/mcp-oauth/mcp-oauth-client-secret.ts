// Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).
//
// The client secret of an entry that names a pre-registered OAuth client is
// not part of the entry: the entry dialog saves it through its own endpoint
// once the entry is saved, and the lists only say whether one is set. These
// hooks give the two MCP pages what the dialog needs for that.

import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  deleteAgentMcpOAuthClientSecret,
  deleteWorkspaceMcpOAuthClientSecret,
  setAgentMcpOAuthClientSecret,
  setWorkspaceMcpOAuthClientSecret,
  useInvalidateMcpOAuth,
} from "@multica/core/mcp-oauth";
import type { Agent } from "@multica/core/types";
import { workspaceMcpServersOptions } from "@multica/core/workspace/queries";
import { useT } from "../i18n";
import { useAgentMcpOAuthList, useWorkspaceMcpOAuthList } from "./mcp-oauth-rows";

/**
 * Runs a client secret write. A failure is shown and rethrown, so the dialog
 * stays open with what was typed; the entry itself is already saved.
 */
function useSecretWrite(wsId: string) {
  const { t } = useT("mcpoauth");
  const invalidate = useInvalidateMcpOAuth(wsId);
  return async (write: () => Promise<void>) => {
    try {
      await write();
    } catch (error) {
      toast.error(
        t(($) => $.client.secret_save_failed, {
          error: error instanceof Error && error.message ? error.message : t(($) => $.action_failed),
        }),
      );
      throw error;
    } finally {
      await invalidate();
    }
  };
}

export interface McpOAuthClientSecretControls {
  /** Whether the saved entry under `name` has a client secret. */
  isSet: (name: string | undefined) => boolean;
  /** Saves (or, with null, removes) the client secret of the entry under `name`. */
  save: (name: string, clientSecret: string | null) => Promise<void>;
}

/** For the agent's own entries, on its MCP tab. */
export function useAgentMcpOAuthClientSecret(agent: Agent, enabled: boolean): McpOAuthClientSecretControls {
  const wsId = agent.workspace_id ?? "";
  const list = useAgentMcpOAuthList(agent, enabled);
  const run = useSecretWrite(wsId);
  return {
    isSet: (name) =>
      name !== undefined &&
      (list.data?.servers?.some((entry) => entry.source !== "workspace" && entry.name === name && entry.client_secret_set) ??
        false),
    save: (name, clientSecret) =>
      run(() =>
        clientSecret === null
          ? deleteAgentMcpOAuthClientSecret(wsId, agent.id, name)
          : setAgentMcpOAuthClientSecret(wsId, agent.id, name, clientSecret),
      ),
  };
}

/** For the workspace MCP library. The entry is found by name: it may have just been created. */
export function useWorkspaceMcpOAuthClientSecret(wsId: string, enabled: boolean): McpOAuthClientSecretControls {
  const qc = useQueryClient();
  const list = useWorkspaceMcpOAuthList(wsId, enabled);
  const run = useSecretWrite(wsId);
  return {
    isSet: (name) =>
      name !== undefined && (list.data?.servers?.some((entry) => entry.name === name && entry.client_secret_set) ?? false),
    save: (name, clientSecret) =>
      run(async () => {
        const library = await qc.fetchQuery({ ...workspaceMcpServersOptions(wsId), staleTime: 0 });
        const server = library.find((item) => item.name === name);
        if (!server) throw new Error(`MCP server "${name}" not found`);
        if (clientSecret === null) await deleteWorkspaceMcpOAuthClientSecret(wsId, server.id);
        else await setWorkspaceMcpOAuthClientSecret(wsId, server.id, clientSecret);
      }),
  };
}
