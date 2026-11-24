// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// Off unless switched on: with the flag off the claim path must behave as
// upstream.
func TestMCPOAuthV1DefaultsOff(t *testing.T) {
	ctx := context.Background()
	if MCPOAuthV1Enabled(ctx, featureflag.NewService(featureflag.NewStaticProvider())) {
		t.Fatal("mcp_oauth_v1 must default to off")
	}
	provider := featureflag.NewStaticProvider()
	provider.Set(MCPOAuthV1, featureflag.Rule{Default: true})
	if !MCPOAuthV1Enabled(ctx, featureflag.NewService(provider)) {
		t.Fatal("mcp_oauth_v1 switched on reads as off")
	}
}
