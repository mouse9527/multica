// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: pre-registered OAuth clients (M-295).
export { McpOAuthCallbackPage } from "./mcp-oauth-callback-page";
export { McpOAuthConnection, useMcpOAuthActions } from "./mcp-oauth-connection";
export { McpOAuthAuthPicker, type McpFormAuth } from "./mcp-oauth-auth";
export {
  AgentMcpOAuthConnection,
  AgentMcpOAuthOwnerNote,
  WorkspaceMcpOAuthConnection,
  useMcpOAuthAvailable,
} from "./mcp-oauth-rows";
export {
  useAgentMcpOAuthClientSecret,
  useWorkspaceMcpOAuthClientSecret,
  type McpOAuthClientSecretControls,
} from "./mcp-oauth-client-secret";
