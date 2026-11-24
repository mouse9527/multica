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

// The web shows the OAuth MCP settings only while mcp_oauth_v1 is on, so the
// flag is published, and published as off by default.
func TestFrontendMCPOAuthFollowsTheFlag(t *testing.T) {
	for _, on := range []bool{true, false} {
		provider := featureflag.NewStaticProvider()
		provider.Set(MCPOAuthV1, featureflag.Rule{Default: on})
		got, ok := EvaluateFrontendPublicFlags(context.Background(), featureflag.NewService(provider))[MCPOAuthV1]
		if !ok || got != on {
			t.Errorf("mcp_oauth_v1=%v: published %v (present %v)", on, got, ok)
		}
	}
	if EvaluateFrontendPublicFlags(context.Background(), featureflag.NewService(featureflag.NewStaticProvider()))[MCPOAuthV1] {
		t.Error("mcp_oauth_v1 must default to off")
	}
}
