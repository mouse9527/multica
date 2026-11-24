// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package protocol

// DaemonCapabilityMCPOAuthProxyV1 advertises that the daemon serves OAuth MCP
// servers to the runtime through a local proxy and fetches the access token
// for each call from GET /api/daemon/tasks/{id}/mcp-oauth/{serverId}/credential.
// A server seeing it sends the servers to proxy (mcp_oauth_proxy) instead of
// putting tokens in mcp_config, so a token expiring mid-task is replaced and
// none is written to the runtime's config files.
const DaemonCapabilityMCPOAuthProxyV1 = "mcp-oauth-proxy-v1"
