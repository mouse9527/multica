// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

import type { z } from "zod";
import type {
  McpOAuthCompletionSchema,
  McpOAuthConnectionSchema,
  McpOAuthServerListSchema,
  McpOAuthServerSchema,
} from "./schemas";

export type McpOAuthConnection = z.infer<typeof McpOAuthConnectionSchema>;
export type McpOAuthServer = z.infer<typeof McpOAuthServerSchema>;

/**
 * `available` is false when the endpoint answers 404 (`mcp_oauth_v1` off, or
 * a server without the feature).
 */
export type McpOAuthServerList = z.infer<typeof McpOAuthServerListSchema> & { available: boolean };

export type McpOAuthCompletion = z.infer<typeof McpOAuthCompletionSchema>;

export interface CreateMcpOAuthServerInput {
  name: string;
  url: string;
  scope?: string;
  /** Sent with every MCP request next to the token; write-only. */
  extraHeaders?: Record<string, string>;
}

export interface CompleteMcpOAuthInput {
  state: string;
  code: string;
  /** The authorization server's `error` parameter when the user declined. */
  error?: string;
}
