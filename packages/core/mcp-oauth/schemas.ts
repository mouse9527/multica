// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

import { z } from "zod";

const nullableString = z.string().nullable().optional().transform((v) => v ?? null);

/** The agent owner's connection to a server. Never carries a token. */
export const McpOAuthConnectionSchema = z.object({
  // "connected" | "needs_reconnect"; anything unknown is shown as needing a
  // reconnect, which is the safe reading.
  status: z.string().catch("needs_reconnect"),
  expires_at: nullableString,
  last_error: z.string().optional().catch(undefined).transform((v) => v ?? ""),
  connected_at: z.string().catch(""),
  refreshed_at: nullableString,
});

/** An OAuth MCP server on an agent. Extra header values are never returned. */
export const McpOAuthServerSchema = z.object({
  id: z.string(),
  name: z.string().catch(""),
  url: z.string().catch(""),
  scope: z.string().catch(""),
  has_extra_headers: z.boolean().catch(false),
  created_at: z.string().catch(""),
  connection: McpOAuthConnectionSchema.nullable().optional().catch(null).transform((v) => v ?? null),
});

export const McpOAuthServerListSchema = z.object({
  servers: z.array(McpOAuthServerSchema).catch([]),
  can_connect: z.boolean().catch(false),
});

export const McpOAuthConnectSchema = z.object({
  authorization_url: z.string().min(1),
});

export const McpOAuthCompletionSchema = z.object({
  workspace_id: z.string(),
  agent_id: z.string().catch(""),
  server_id: z.string().catch(""),
  return_to: z.string().catch(""),
});
