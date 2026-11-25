// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth entries in MCP configs (M-269).

package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// ErrInvalidInput wraps every validation failure of an OAuth MCP entry.
var ErrInvalidInput = errors.New("invalid MCP OAuth server")

const maxScopeBytes = 1024

// ValidateServerURL checks the shape of an MCP server URL as stored. Whether
// its host may be reached is decided each time it is contacted, because DNS
// can change in between.
func ValidateServerURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 9 || len(raw) > 2048 {
		return nil, fmt.Errorf("%w: url must be 9 to 2048 characters", ErrInvalidInput)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: url must be HTTPS without userinfo, query or fragment", ErrInvalidInput)
	}
	return u, nil
}

func validateScope(scope string) error {
	if len(scope) > maxScopeBytes || strings.ContainsAny(scope, "\r\n\x00\"\\") {
		return fmt.Errorf("%w: scope is too long or has invalid characters", ErrInvalidInput)
	}
	return nil
}

// EntryURLAllowed applies, when an entry is saved, the address rules that
// need no DNS lookup: the URL must be public HTTPS, so localhost and a
// literal private, loopback or link-local address are refused unless the
// operator listed that exact origin (AllowedOriginsEnv) or it is a
// development origin. A host name passes here; what it resolves to is
// checked each time it is contacted (Flow, Refresh), because DNS can change
// in between.
func EntryURLAllowed(ctx context.Context, rawURL string) error {
	if _, err := remotemcp.ValidatePublicHTTPSEndpoint(withAllowedOrigins(ctx), rawURL, nil, literalOnlyResolver{}); err != nil {
		return fmt.Errorf("%w: url must be a public HTTPS address, or an origin the operator allowed", ErrInvalidInput)
	}
	return nil
}

// literalOnlyResolver answers an IP literal with itself and any host name
// with a public placeholder address, so the save-time check judges literals
// and leaves names to contact time without making a DNS query.
type literalOnlyResolver struct{}

var publicPlaceholder = netip.MustParseAddr("1.1.1.1")

func (literalOnlyResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{addr}, nil
	}
	if host == "" {
		return nil, &net.DNSError{Err: "no host", Name: host}
	}
	return []netip.Addr{publicPlaceholder}, nil
}
