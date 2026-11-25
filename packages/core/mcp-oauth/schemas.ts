// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

import { z } from "zod";

const nullableString = z.string().nullable().optional().transform((v) => v ?? null);

/** A user's connection to an OAuth MCP entry. Never carries a token. */
export const McpOAuthConnectionSchema = z.object({
  // "connected" | "needs_reconnect"; anything unknown is shown as needing a
  // reconnect, which is the safe reading.
  status: z.string().catch("needs_reconnect"),
  expires_at: nullableString,
  last_error: z.string().optional().catch(undefined).transform((v) => v ?? ""),
  connected_at: z.string().catch(""),
  refreshed_at: nullableString,
});

const connection = McpOAuthConnectionSchema.nullable().optional().catch(null).transform((v) => v ?? null);

/**
 * An OAuth entry an agent runs with: one of its own MCP entries, or a
 * workspace MCP library entry bound to it. URLs are never returned.
 */
export const McpOAuthAgentEntrySchema = z.object({
  name: z.string(),
  // "agent" | "workspace"; an unknown source is treated as the agent's own.
  source: z.string().catch("agent"),
  workspace_server_id: z.string().optional().catch(undefined).transform((v) => v ?? ""),
  scope: z.string().catch(""),
  /** The connection is the owner's one connection to a workspace entry, shared by all their agents. */
  shared: z.boolean().catch(false),
  /** Why the entry cannot be connected; empty when it can. */
  problem: z.string().optional().catch(undefined).transform((v) => v ?? ""),
  connection,
});

export const McpOAuthAgentListSchema = z.object({
  servers: z.array(McpOAuthAgentEntrySchema).catch([]),
  can_connect: z.boolean().catch(false),
});

/** A workspace MCP library entry that uses OAuth, with the caller's own connection. */
export const McpOAuthWorkspaceEntrySchema = z.object({
  server_id: z.string(),
  name: z.string().catch(""),
  scope: z.string().catch(""),
  problem: z.string().optional().catch(undefined).transform((v) => v ?? ""),
  connection,
});

export const McpOAuthWorkspaceListSchema = z.object({
  servers: z.array(McpOAuthWorkspaceEntrySchema).catch([]),
});

export const McpOAuthConnectSchema = z.object({
  authorization_url: z.string().min(1),
});

export const McpOAuthCompletionSchema = z.object({
  workspace_id: z.string(),
  agent_id: z.string().catch(""),
  server_id: z.string().catch(""),
  server_ref: z.string().catch(""),
  return_to: z.string().catch(""),
});
