// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"context"
	"log/slog"
	"os"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// AllowedOriginsEnv names exact origins an OAuth MCP server, and the
// authorization server it advertises, may use even though they resolve to a
// non-public address, such as a self-hosted gateway on a private network.
// Comma separated, each entry exactly "https://host" or "https://host:port".
// Empty (the default) allows nothing beyond the public internet. It is set by
// the operator, never through the API.
//
// It relaxes one rule only, "the address must be public", and only for the
// calls this package makes (discovery, registration, code exchange,
// refresh). HTTPS, certificate verification against the system roots, no
// redirects and no proxy still apply. Plugins are not affected.
const AllowedOriginsEnv = "MULTICA_MCP_OAUTH_ALLOWED_ORIGINS"

// withAllowedOrigins attaches the operator's allowed origins to ctx for the
// pkg/remotemcp calls made with it. An invalid value allows nothing and is
// logged, rather than allowing something broader than written.
func withAllowedOrigins(ctx context.Context) context.Context {
	raw := os.Getenv(AllowedOriginsEnv)
	if raw == "" {
		return ctx
	}
	origins, err := remotemcp.ParseAllowedOrigins(raw)
	if err != nil {
		slog.Warn("mcp oauth: "+AllowedOriginsEnv+" is invalid; allowing no extra origins", "error", err)
		return ctx
	}
	return remotemcp.WithAllowedOrigins(ctx, origins)
}
