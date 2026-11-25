// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

export * from "./types";
export {
  completeMcpOAuth,
  connectAgentMcpOAuth,
  connectWorkspaceMcpOAuth,
  disconnectAgentMcpOAuth,
  disconnectWorkspaceMcpOAuth,
  listAgentMcpOAuth,
  listWorkspaceMcpOAuth,
} from "./api";
export {
  agentMcpOAuthOptions,
  mcpOAuthKeys,
  useConnectAgentMcpOAuth,
  useConnectWorkspaceMcpOAuth,
  useDisconnectAgentMcpOAuth,
  useDisconnectWorkspaceMcpOAuth,
  useInvalidateMcpOAuth,
  workspaceMcpOAuthOptions,
} from "./queries";

/** Frontend flag: the server's mcp_oauth_v1. */
export const MCP_OAUTH_FLAG = "mcp_oauth_v1";

/** The app page the authorization server returns to (server: mcpoauth.CallbackPath). */
export const MCP_OAUTH_CALLBACK_PATH = "/oauth/mcp";

/** The value of an MCP entry's `auth.type` that asks for OAuth (server: mcpoauth.AuthTypeOAuth). */
export const MCP_OAUTH_AUTH_TYPE = "oauth";
