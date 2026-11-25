// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

import type { z } from "zod";
import type {
  McpOAuthAgentEntrySchema,
  McpOAuthAgentListSchema,
  McpOAuthCompletionSchema,
  McpOAuthConnectionSchema,
  McpOAuthWorkspaceEntrySchema,
  McpOAuthWorkspaceListSchema,
} from "./schemas";

export type McpOAuthConnection = z.infer<typeof McpOAuthConnectionSchema>;
export type McpOAuthAgentEntry = z.infer<typeof McpOAuthAgentEntrySchema>;
export type McpOAuthWorkspaceEntry = z.infer<typeof McpOAuthWorkspaceEntrySchema>;

/**
 * `available` is false when the endpoint answers 404 (`mcp_oauth_v1` off, or
 * a server without the feature).
 */
export type McpOAuthAgentList = z.infer<typeof McpOAuthAgentListSchema> & { available: boolean };
export type McpOAuthWorkspaceList = z.infer<typeof McpOAuthWorkspaceListSchema> & { available: boolean };

export type McpOAuthCompletion = z.infer<typeof McpOAuthCompletionSchema>;

/** Connected, needs a reconnect (expired, revoked, or the entry's URL changed), or never connected. */
export type McpOAuthConnectionState = "connected" | "needs_reconnect" | "not_connected";

export function mcpOAuthConnectionState(connection: McpOAuthConnection | null): McpOAuthConnectionState {
  if (!connection) return "not_connected";
  return connection.status === "connected" ? "connected" : "needs_reconnect";
}

export interface CompleteMcpOAuthInput {
  state: string;
  code: string;
  /** The authorization server's `error` parameter when the user declined. */
  error?: string;
}
