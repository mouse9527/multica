// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: parse the allowed origins once per value (M-279).

package mcpoauth

import (
	"context"
	"log/slog"
	"os"
	"sync"

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
	origins, ok := allowedOrigins(slog.Default())
	if !ok {
		return ctx
	}
	return remotemcp.WithAllowedOrigins(ctx, origins)
}

// allowedOriginsCache holds the parse of the last value seen. Every
// discovery, code exchange and refresh — including the daemon's per-call
// credential fetches — reads the variable, so an invalid value is reported
// once rather than on every call; a changed value is parsed (and, if still
// invalid, reported) again.
var allowedOriginsCache struct {
	sync.Mutex
	parsed  bool
	raw     string
	origins remotemcp.AllowedOrigins
	err     error
}

// allowedOrigins returns the parsed AllowedOriginsEnv, false when it is
// empty or invalid. logger receives the warning for an invalid value.
func allowedOrigins(logger *slog.Logger) (remotemcp.AllowedOrigins, bool) {
	raw := os.Getenv(AllowedOriginsEnv)
	if raw == "" {
		return remotemcp.AllowedOrigins{}, false
	}
	allowedOriginsCache.Lock()
	defer allowedOriginsCache.Unlock()
	if !allowedOriginsCache.parsed || allowedOriginsCache.raw != raw {
		origins, err := remotemcp.ParseAllowedOrigins(raw)
		if err != nil {
			logger.Warn("mcp oauth: "+AllowedOriginsEnv+" is invalid; allowing no extra origins", "error", err)
		}
		allowedOriginsCache.parsed, allowedOriginsCache.raw = true, raw
		allowedOriginsCache.origins, allowedOriginsCache.err = origins, err
	}
	if allowedOriginsCache.err != nil {
		return remotemcp.AllowedOrigins{}, false
	}
	return allowedOriginsCache.origins, true
}
