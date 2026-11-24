// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

export * from "./types";
export {
  completeMcpOAuth,
  connectMcpOAuthServer,
  createMcpOAuthServer,
  deleteMcpOAuthServer,
  disconnectMcpOAuthServer,
  listMcpOAuthServers,
} from "./api";
export {
  mcpOAuthKeys,
  mcpOAuthServersOptions,
  useConnectMcpOAuthServer,
  useCreateMcpOAuthServer,
  useDeleteMcpOAuthServer,
  useDisconnectMcpOAuthServer,
} from "./queries";

/** Frontend flag: the server's mcp_oauth_v1. */
export const MCP_OAUTH_FLAG = "mcp_oauth_v1";

/** The app page the authorization server returns to (server: mcpoauth.CallbackPath). */
export const MCP_OAUTH_CALLBACK_PATH = "/oauth/mcp";
