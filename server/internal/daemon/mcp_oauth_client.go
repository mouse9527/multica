// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ResolveMCPOAuthCredential fetches the current token for an OAuth MCP server
// of a running task. It authenticates as this daemon; the server serves only
// a task this daemon may access that is still dispatched or running.
//
// rejectedHash, when set, names a token the MCP server just refused (hex
// SHA-256): the server then refreshes it rather than serving it again.
func (c *Client) ResolveMCPOAuthCredential(ctx context.Context, taskID, serverID, rejectedHash string) (mcpOAuthCredential, error) {
	var response struct {
		Headers   map[string]string `json:"headers"`
		ExpiresAt *time.Time        `json:"expires_at"`
	}
	path := fmt.Sprintf("/api/daemon/tasks/%s/mcp-oauth/%s/credential", url.PathEscape(taskID), url.PathEscape(serverID))
	if rejectedHash != "" {
		path += "?rejected=" + url.QueryEscape(rejectedHash)
	}
	if err := c.getJSON(ctx, path, &response); err != nil {
		return mcpOAuthCredential{}, err
	}
	if len(response.Headers) == 0 {
		return mcpOAuthCredential{}, errors.New("MCP OAuth credential response has no headers")
	}
	credential := mcpOAuthCredential{Headers: response.Headers}
	if response.ExpiresAt != nil {
		credential.ExpiresAt = *response.ExpiresAt
	}
	return credential, nil
}
