// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
import { McpOAuthCallbackPage } from "@multica/views/mcp-oauth";

/**
 * `/oauth/mcp` — where an OAuth MCP server's authorization server sends the
 * browser after the agent owner connects it (the server registers this path
 * as the redirect URI: mcpoauth.CallbackPath).
 *
 * A top-level route: one redirect URI serves every workspace, and the page
 * learns the workspace from the server's answer. `oauth` is already a
 * reserved slug, so this cannot shadow a real workspace.
 *
 * Desktop has no route of its own here: connecting opens the system browser,
 * so the return always lands on the web app, where the user must be signed in.
 */
export default function McpOAuthCallbackRoute() {
  return <McpOAuthCallbackPage />;
}
