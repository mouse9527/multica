// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package redact

import (
	"strings"
	"testing"
)

// An OAuth MCP server reaches a runtime as an `Authorization: Bearer` header
// in the MCP config. If an agent prints that config, or the runtime echoes a
// request, the token must not survive into the transcript. The generic Bearer
// rule covers it; these pin the shapes the token actually takes, including the
// opaque session tokens an MCP gateway issues, which are not JWTs.
func TestRedactMCPOAuthBearerShapes(t *testing.T) {
	t.Parallel()
	const token = "llm_session_9f8E7d6C5b4A3z2Y1x0W-v.u~t+s/r"
	for name, input := range map[string]string{
		"claude mcp-config JSON": `{"mcpServers":{"gateway":{"type":"http","url":"https://mcp.example.com/mcp","headers":{"Authorization":"Bearer ` + token + `"}}}}`,
		"codex config.toml":      `[mcp_servers.gateway]` + "\n" + `url = "https://mcp.example.com/mcp"` + "\n" + `http_headers = { Authorization = "Bearer ` + token + `" }`,
		"JSON escaped in a log":  `"{\"headers\":{\"Authorization\":\"Bearer ` + token + `\"}}"`,
		"HTTP request dump":      "POST /mcp HTTP/1.1\r\nauthorization: bearer " + token + "\r\n",
		"curl command":           `curl -H 'Authorization: Bearer ` + token + `' https://mcp.example.com/mcp`,
	} {
		got := Text(input)
		if strings.Contains(got, "9f8E7d6C5b4A3z2Y1x0W") {
			t.Errorf("%s: token survived: %s", name, got)
		}
		if !strings.Contains(strings.ToLower(got), "bearer [redacted]") {
			t.Errorf("%s: expected the Bearer placeholder: %s", name, got)
		}
	}
}
