// Added by the mouse9527/multica fork: manual OAuth endpoints, resource and audience for MCP entries (M-295 stage 2).

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// ServerConfig is how an entry overrides what the connect would otherwise
// discover about its authorization server:
//
//	"auth": {"type": "oauth", ...,
//	         "authorization_endpoint": "https://...", "token_endpoint": "https://...",
//	         "resource": "...", "audience": "..."}
//
// All optional. How the authorization server is found, first match wins:
// manual endpoints (both together), then the issuer (ClientConfig.Issuer,
// its RFC 8414 / OIDC metadata), then discovery from the MCP server's
// protected resource metadata (RFC 9728).
type ServerConfig struct {
	AuthorizationEndpoint string
	TokenEndpoint         string
	// Resource replaces the RFC 8707 resource indicator, which is otherwise
	// the entry URL, in the authorization, code and refresh requests. The
	// token is still only ever sent to the entry URL.
	Resource string
	// Audience is sent as the "audience" parameter of the authorization
	// request, which authorization servers in the Auth0 style require.
	Audience string
}

// ManualEndpoints reports whether the entry names its endpoints itself.
func (s ServerConfig) ManualEndpoints() bool { return s.AuthorizationEndpoint != "" }

// Hosts are the host names of the manual endpoints, shown before connecting:
// the browser goes to the first, the server sends the code (and any client
// secret) to the second. Empty without manual endpoints.
func (s ServerConfig) Hosts() (authorization, token string) {
	if !s.ManualEndpoints() {
		return "", ""
	}
	return hostOf(s.AuthorizationEndpoint), hostOf(s.TokenEndpoint)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

const (
	maxEndpointBytes = 2048
	maxResourceBytes = 2048
	maxAudienceBytes = 1024
)

// ErrEndpointNotAllowed: a manual endpoint is not a public HTTPS address,
// nor an origin the operator allowed. Manual endpoints pass the same checks
// as discovered ones.
var ErrEndpointNotAllowed = errors.New("a configured OAuth endpoint is not an allowed public HTTPS address")

// ReasonServerChanged: the entry now names other endpoints, another
// resource or audience than the grant was made with.
const ReasonServerChanged = "server_changed"

// parseServerField reads one of the server keys of an entry's auth object
// into s. It reports false for a key that is not a server key.
func parseServerField(key string, value json.RawMessage, s *ServerConfig) (bool, error) {
	var target *string
	switch key {
	case "authorization_endpoint":
		target = &s.AuthorizationEndpoint
	case "token_endpoint":
		target = &s.TokenEndpoint
	case "resource":
		target = &s.Resource
	case "audience":
		target = &s.Audience
	default:
		return false, nil
	}
	var text string
	if json.Unmarshal(value, &text) != nil {
		return true, fmt.Errorf("%w: auth.%s must be a string", ErrInvalidInput, key)
	}
	text = strings.TrimSpace(text)
	var err error
	switch key {
	case "authorization_endpoint", "token_endpoint":
		err = validateEndpoint(key, text)
	case "resource":
		err = validateResource(text)
	case "audience":
		err = validateParamValue("audience", text, maxAudienceBytes)
	}
	if err != nil {
		return true, err
	}
	*target = text
	return true, nil
}

// validateAuthorization checks the client and server keys against each
// other.
func validateAuthorization(c ClientConfig, s ServerConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	if (s.AuthorizationEndpoint == "") != (s.TokenEndpoint == "") {
		return fmt.Errorf("%w: auth.authorization_endpoint and auth.token_endpoint go together", ErrInvalidInput)
	}
	// Without discovery there is no registration endpoint to register a
	// client at.
	if s.ManualEndpoints() && !c.Preregistered() {
		return fmt.Errorf("%w: manual endpoints need a pre-registered client (auth.client_id)", ErrInvalidInput)
	}
	return nil
}

// validateEndpoint checks the shape of a manual endpoint: HTTPS, a host, no
// userinfo or fragment. A query is allowed (RFC 6749 §3.1, §3.2). Whether its
// host may be reached is decided each time it is contacted.
func validateEndpoint(field, raw string) error {
	u, err := url.Parse(raw)
	if raw == "" || len(raw) > maxEndpointBytes || err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" || !isVSChars(raw) {
		return fmt.Errorf("%w: auth.%s must be an HTTPS URL without userinfo or fragment", ErrInvalidInput, field)
	}
	return nil
}

// validateResource checks an RFC 8707 resource indicator: an absolute URI
// without a fragment (§2). Its length fits the connection tables.
func validateResource(raw string) error {
	u, err := url.Parse(raw)
	if len(raw) < 9 || len(raw) > maxResourceBytes || err != nil || u.Scheme == "" || u.Fragment != "" ||
		strings.Contains(raw, "#") || !isVSChars(raw) || strings.Contains(raw, " ") {
		return fmt.Errorf("%w: auth.resource must be an absolute URI of 9 to %d characters without a fragment", ErrInvalidInput, maxResourceBytes)
	}
	return nil
}

// validateParamValue applies validateScope's character rule to a value sent
// as an authorization request parameter, and requires one.
func validateParamValue(field, value string, max int) error {
	if value == "" || len(value) > max || strings.ContainsAny(value, "\r\n\x00\"\\") {
		return fmt.Errorf("%w: auth.%s must be 1 to %d characters without quotes, backslashes or line breaks", ErrInvalidInput, field, max)
	}
	return nil
}

// endpointAllowed is EntryURLAllowed for a manual endpoint, whose query is
// not part of the address.
func endpointAllowed(ctx context.Context, field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: auth.%s must be an HTTPS URL", ErrInvalidInput, field)
	}
	u.RawQuery, u.ForceQuery = "", false
	if _, err := remotemcp.ValidatePublicHTTPSEndpoint(withAllowedOrigins(ctx), u.String(), nil, literalOnlyResolver{}); err != nil {
		return fmt.Errorf("%w: auth.%s must be a public HTTPS address, or an origin the operator allowed", ErrInvalidInput, field)
	}
	return nil
}

// grantMismatch is why a grant made with clientID, authMethod,
// tokenEndpoint and resource may not serve an entry configured as client and
// server now: ReasonClientChanged, ReasonServerChanged, or "" when it may.
func grantMismatch(client ClientConfig, server ServerConfig, clientID, authMethod, tokenEndpoint, resource string) string {
	if !client.clientMatches(clientID, authMethod) {
		return ReasonClientChanged
	}
	if server.TokenEndpoint != "" && tokenEndpoint != server.TokenEndpoint {
		return ReasonServerChanged
	}
	if server.Resource != "" && resource != server.Resource {
		return ReasonServerChanged
	}
	return ""
}

// AuthorizationChange is why saving after over before takes the grants of
// an entry that uses OAuth both times out of use: ReasonClientChanged (its
// client), ReasonServerChanged (its endpoints, issuer, resource or
// audience), or "" when neither changed. The scope does not count: a grant
// keeps the scope it was given.
func AuthorizationChange(before, after json.RawMessage) string {
	if !IsOAuthEntry(before) || !IsOAuthEntry(after) {
		return ""
	}
	old, oldErr := ParseEntry(before)
	next, nextErr := ParseEntry(after)
	if oldErr != nil || nextErr != nil {
		return ""
	}
	switch {
	case old.Client.ID != next.Client.ID || old.Client.TokenEndpointAuthMethod != next.Client.TokenEndpointAuthMethod:
		return ReasonClientChanged
	case old.Client != next.Client || old.Server != next.Server:
		return ReasonServerChanged
	}
	return ""
}

// discover finds the target's authorization server: its manual endpoints,
// else its issuer's metadata, else RFC 9728 discovery. Every endpoint passes
// pkg/remotemcp's public-HTTPS checks and the operator's allowlist; a
// resource override replaces the resource indicator.
func discover(ctx context.Context, target Target) (remotemcp.OAuthMetadata, error) {
	var (
		metadata remotemcp.OAuthMetadata
		err      error
	)
	switch {
	case target.Server.ManualEndpoints():
		metadata = remotemcp.OAuthMetadata{
			ResourceEndpoint:      target.URL,
			AuthorizationEndpoint: target.Server.AuthorizationEndpoint,
			TokenEndpoint:         target.Server.TokenEndpoint,
		}
		if err := remotemcp.ValidateOAuthMetadata(ctx, metadata); err != nil {
			return remotemcp.OAuthMetadata{}, fmt.Errorf("%w: %w", ErrEndpointNotAllowed, err)
		}
	case target.Client.Issuer != "":
		metadata, err = remotemcp.DiscoverOAuthForIssuer(ctx, target.URL, target.Client.Issuer)
		if errors.Is(err, remotemcp.ErrAuthorizationServerMismatch) {
			return remotemcp.OAuthMetadata{}, fmt.Errorf("%w: %w", ErrIssuerMismatch, err)
		}
		if err != nil {
			return remotemcp.OAuthMetadata{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
		}
	default:
		metadata, err = remotemcp.DiscoverOAuth(ctx, target.URL, nil)
		if err != nil {
			return remotemcp.OAuthMetadata{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
		}
	}
	if target.Server.Resource != "" {
		metadata.ResourceEndpoint = target.Server.Resource
	}
	return metadata, nil
}

// withAudience adds the audience parameter to an authorization URL.
func withAudience(authorizationURL, audience string) (string, error) {
	if audience == "" {
		return authorizationURL, nil
	}
	u, err := url.Parse(authorizationURL)
	if err != nil {
		return "", err
	}
	query := u.Query()
	query.Set("audience", audience)
	u.RawQuery = query.Encode()
	return u.String(), nil
}
