// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// MCPOAuthV1 gates the fork's server-hosted OAuth 2.1 MCP (FF_MCP_OAUTH_V1):
// the connect routes, the settings entry, and the claim-time injection of
// access tokens. Off by default; off means upstream behavior, and the claim
// path makes no extra call.
const MCPOAuthV1 = "mcp_oauth_v1"

func MCPOAuthV1Enabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, MCPOAuthV1, false)
}
