// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package remotemcp

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// AllowedOrigins is a set of exact origins ("https://host[:port]") that may
// be reached even though they resolve to a non-public address — for example
// a self-hosted MCP gateway on a home network. The zero value allows nothing.
//
// It relaxes exactly one rule, "the address must be public", and only for
// calls whose context carries it (WithAllowedOrigins). HTTPS, the system
// certificate roots, the host pin, no redirects and no proxy still apply.
// Because it travels in the context, only the caller that attached it is
// affected: Plugin discovery and the daemon broker never see it.
type AllowedOrigins struct {
	set map[string]struct{}
}

// ParseAllowedOrigins parses a comma-separated list of exact HTTPS origins.
// An invalid entry fails the whole value, so a typo cannot leave an operator
// believing an origin is allowed, nor allow something broader than written.
func ParseAllowedOrigins(raw string) (AllowedOrigins, error) {
	out := AllowedOrigins{set: map[string]struct{}{}}
	var bad []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		origin, ok := parseOriginEntry(entry)
		if !ok {
			bad = append(bad, entry)
			continue
		}
		out.set[origin] = struct{}{}
	}
	if len(bad) > 0 {
		return AllowedOrigins{}, fmt.Errorf("invalid allowed origins %q: each must be https://host or https://host:port", bad)
	}
	return out, nil
}

func parseOriginEntry(entry string) (string, bool) {
	u, err := url.Parse(entry)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		strings.Contains(u.Hostname(), "*") {
		return "", false
	}
	return canonicalOrigin(u)
}

// canonicalOrigin is "https://host[:port]" with the host lowercased, a
// trailing dot dropped and the default port omitted, so equal origins compare
// equal however they were written.
func canonicalOrigin(u *url.URL) (string, bool) {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return "", false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", false
	}
	port := u.Port()
	if port == "443" {
		port = ""
	}
	if port != "" {
		return "https://" + net.JoinHostPort(host, port), true
	}
	if strings.Contains(host, ":") {
		return "https://[" + host + "]", true
	}
	return "https://" + host, true
}

// Allows reports whether u's origin is exactly one of the set's.
func (o AllowedOrigins) Allows(u *url.URL) bool {
	if len(o.set) == 0 {
		return false
	}
	origin, ok := canonicalOrigin(u)
	if !ok {
		return false
	}
	_, found := o.set[origin]
	return found
}

// Len is the number of origins in the set.
func (o AllowedOrigins) Len() int { return len(o.set) }

type allowedOriginsKey struct{}

// WithAllowedOrigins returns ctx carrying origins for the endpoint checks
// below. Calls made with any other context are unaffected.
func WithAllowedOrigins(ctx context.Context, origins AllowedOrigins) context.Context {
	if origins.Len() == 0 {
		return ctx
	}
	return context.WithValue(ctx, allowedOriginsKey{}, origins)
}

// originAllowedByContext is the hook ValidatePublicHTTPSEndpoint and the
// secure client's dialer consult before refusing a non-public address.
func originAllowedByContext(ctx context.Context, endpoint *url.URL) bool {
	if ctx == nil {
		return false
	}
	origins, ok := ctx.Value(allowedOriginsKey{}).(AllowedOrigins)
	return ok && origins.Allows(endpoint)
}
