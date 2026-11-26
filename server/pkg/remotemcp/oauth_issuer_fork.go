// Added by the mouse9527/multica fork: pre-registered OAuth clients bound to their authorization server (M-295).
// Modified for the mouse9527/multica fork: an issuer works without protected resource metadata (M-295 stage 2).

package remotemcp

import (
	"context"
	"errors"
	"fmt"
)

// ErrAuthorizationServerMismatch: the MCP server's protected resource
// metadata does not name the authorization server a pre-registered client
// belongs to, or that server's metadata states another issuer.
var ErrAuthorizationServerMismatch = errors.New("the MCP server's authorization server is not the one the client is registered with")

// DiscoverOAuthForIssuer is DiscoverOAuth for an entry that names its
// authorization server, issuer: the metadata is fetched from issuer's own
// well-known URLs and must state exactly that issuer (RFC 8414 §3.3, OpenID
// Connect Discovery §4.3), so credentials registered with issuer only ever
// go to issuer's endpoints, as the MCP authorization spec requires
// ("Authorization Server Binding"). The MCP server's protected resource
// metadata (RFC 9728) is optional here — servers without it are what a named
// issuer is for — but when it is published and lists authorization servers,
// issuer must be one of them: the resource moved to another server, and
// credentials of the old one must not be used. Identifiers are compared as
// strings, without normalization.
//
// Every fetched URL passes the same public-HTTPS check as DiscoverOAuth, and
// the result is checked by ValidateOAuthMetadata.
func DiscoverOAuthForIssuer(ctx context.Context, rawEndpoint, issuer string) (OAuthMetadata, error) {
	endpoint, err := ValidatePublicHTTPSEndpoint(ctx, rawEndpoint, nil, nil)
	if err != nil {
		return OAuthMetadata{}, err
	}
	metadataURL := probeResourceMetadataURL(ctx, endpoint)
	if metadataURL == "" {
		metadataURL = protectedResourceMetadataURL(endpoint)
	}
	var resource protectedResourceMetadata
	if resourceURL, err := validateOAuthURL(ctx, metadataURL); err == nil && getOAuthJSON(ctx, resourceURL, &resource) == nil {
		if resource.Resource != "" && resource.Resource != endpoint.String() {
			return OAuthMetadata{}, errors.New("protected resource metadata does not match the MCP endpoint")
		}
		if len(resource.AuthorizationServers) > 0 && !containsString(resource.AuthorizationServers, issuer) {
			return OAuthMetadata{}, ErrAuthorizationServerMismatch
		}
	} else {
		resource = protectedResourceMetadata{}
	}

	issuerURL, err := validateOAuthURL(ctx, issuer)
	if err != nil {
		return OAuthMetadata{}, fmt.Errorf("authorization server: %w", err)
	}
	var server struct {
		authorizationServerMetadata
		Issuer string `json:"issuer"`
	}
	var discoveryErr error
	for _, candidate := range authorizationMetadataURLs(issuerURL) {
		candidateURL, validateErr := validateOAuthURL(ctx, candidate)
		if validateErr != nil {
			discoveryErr = validateErr
			continue
		}
		server.authorizationServerMetadata = authorizationServerMetadata{}
		server.Issuer = ""
		if loadErr := getOAuthJSON(ctx, candidateURL, &server); loadErr != nil {
			// A document that spoke for another server is the finding that
			// matters; a later candidate that is merely absent does not hide it.
			if !errors.Is(discoveryErr, ErrAuthorizationServerMismatch) {
				discoveryErr = loadErr
			}
			continue
		}
		if server.Issuer != issuer {
			// The document speaks for another server; it must not be used.
			discoveryErr = ErrAuthorizationServerMismatch
			continue
		}
		discoveryErr = nil
		break
	}
	if discoveryErr != nil {
		return OAuthMetadata{}, fmt.Errorf("load authorization server metadata: %w", discoveryErr)
	}
	if server.AuthorizationEndpoint == "" || server.TokenEndpoint == "" {
		return OAuthMetadata{}, errors.New("authorization server metadata is missing required endpoints")
	}
	if len(server.CodeChallengeMethodsSupported) > 0 && !containsString(server.CodeChallengeMethodsSupported, "S256") {
		return OAuthMetadata{}, errors.New("authorization server does not support PKCE S256")
	}
	metadata := OAuthMetadata{
		ResourceEndpoint:      endpoint.String(),
		AuthorizationEndpoint: server.AuthorizationEndpoint,
		TokenEndpoint:         server.TokenEndpoint,
		RegistrationEndpoint:  server.RegistrationEndpoint,
		Scopes:                append([]string(nil), resource.ScopesSupported...),
		TokenAuthMethods:      append([]string(nil), server.TokenEndpointAuthMethodsSupport...),
	}
	if err := ValidateOAuthMetadata(ctx, metadata); err != nil {
		return OAuthMetadata{}, fmt.Errorf("authorization server endpoint: %w", err)
	}
	return metadata, nil
}
