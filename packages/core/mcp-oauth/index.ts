// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: pre-registered OAuth clients (M-295).

export * from "./types";
export {
  completeMcpOAuth,
  connectAgentMcpOAuth,
  connectWorkspaceMcpOAuth,
  deleteAgentMcpOAuthClientSecret,
  deleteWorkspaceMcpOAuthClientSecret,
  disconnectAgentMcpOAuth,
  disconnectWorkspaceMcpOAuth,
  listAgentMcpOAuth,
  listWorkspaceMcpOAuth,
  setAgentMcpOAuthClientSecret,
  setWorkspaceMcpOAuthClientSecret,
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

/** Token endpoint auth methods an entry may ask for (server: mcpoauth.AuthMethod*). */
export const MCP_OAUTH_TOKEN_AUTH_METHODS = ["client_secret_basic", "client_secret_post", "none"] as const;
export type McpOAuthTokenAuthMethod = (typeof MCP_OAUTH_TOKEN_AUTH_METHODS)[number];
