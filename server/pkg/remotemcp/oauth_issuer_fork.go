// Added by the mouse9527/multica fork: pre-registered OAuth clients bound to their authorization server (M-295).
// Modified for the mouse9527/multica fork: an issuer works without protected resource metadata (M-295 stage 2).
// Modified for the mouse9527/multica fork: validated issuer and RFC 9207 support for every discovery (M-295 stage 2).

package remotemcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// ErrAuthorizationServerMismatch: the MCP server's protected resource
// metadata does not name the authorization server a pre-registered client
// belongs to, or that server's metadata states another issuer.
var ErrAuthorizationServerMismatch = errors.New("the MCP server's authorization server is not the one the client is registered with")

// AuthorizationServer is OAuthMetadata together with what the MCP
// authorization spec requires a client to record before sending the browser
// to the authorization server: the issuer of the validated metadata, and
// whether the server sends RFC 9207's iss parameter in its authorization
// responses (authorization_response_iss_parameter_supported).
type AuthorizationServer struct {
	OAuthMetadata
	Issuer                string
	ISSParameterSupported bool
}

// DiscoverOAuthServer is DiscoverOAuth with the issuer check RFC 8414 §3.3
// and the MCP authorization spec require ("the issuer value in the document
// MUST be identical to the issuer identifier used to construct the
// well-known URL; if they differ, the client MUST NOT use the metadata"): the
// authorization server is the first the protected resource metadata (RFC
// 9728, required here as in DiscoverOAuth) names, and its metadata must state
// exactly that issuer. DiscoverOAuth itself is upstream's and stays as it is
// for its other callers.
func DiscoverOAuthServer(ctx context.Context, rawEndpoint string) (AuthorizationServer, error) {
	endpoint, err := ValidatePublicHTTPSEndpoint(ctx, rawEndpoint, nil, nil)
	if err != nil {
		return AuthorizationServer{}, err
	}
	resourceURL, err := validateOAuthURL(ctx, resourceMetadataURLFor(ctx, endpoint))
	if err != nil {
		return AuthorizationServer{}, fmt.Errorf("protected resource metadata URL: %w", err)
	}
	var resource protectedResourceMetadata
	if err := getOAuthJSON(ctx, resourceURL, &resource); err != nil {
		return AuthorizationServer{}, fmt.Errorf("load protected resource metadata: %w", err)
	}
	if resource.Resource != "" && resource.Resource != endpoint.String() {
		return AuthorizationServer{}, errors.New("protected resource metadata does not match the MCP endpoint")
	}
	if len(resource.AuthorizationServers) == 0 {
		return AuthorizationServer{}, errors.New("protected resource metadata does not advertise an authorization server")
	}
	return loadAuthorizationServer(ctx, endpoint, resource.AuthorizationServers[0], resource.ScopesSupported)
}

// DiscoverOAuthForIssuer is DiscoverOAuthServer for an entry that names its
// authorization server, issuer: the metadata is fetched from issuer's own
// well-known URLs and must state exactly that issuer, so credentials
// registered with issuer only ever go to issuer's endpoints, as the MCP
// authorization spec requires ("Authorization Server Binding"). The MCP
// server's protected resource metadata is optional here — servers without it
// are what a named issuer is for — but when it is published and lists
// authorization servers, issuer must be one of them: the resource moved to
// another server, and credentials of the old one must not be used.
func DiscoverOAuthForIssuer(ctx context.Context, rawEndpoint, issuer string) (AuthorizationServer, error) {
	endpoint, err := ValidatePublicHTTPSEndpoint(ctx, rawEndpoint, nil, nil)
	if err != nil {
		return AuthorizationServer{}, err
	}
	var resource protectedResourceMetadata
	if resourceURL, err := validateOAuthURL(ctx, resourceMetadataURLFor(ctx, endpoint)); err == nil && getOAuthJSON(ctx, resourceURL, &resource) == nil {
		if resource.Resource != "" && resource.Resource != endpoint.String() {
			return AuthorizationServer{}, errors.New("protected resource metadata does not match the MCP endpoint")
		}
		if len(resource.AuthorizationServers) > 0 && !containsString(resource.AuthorizationServers, issuer) {
			return AuthorizationServer{}, ErrAuthorizationServerMismatch
		}
	} else {
		resource = protectedResourceMetadata{}
	}
	return loadAuthorizationServer(ctx, endpoint, issuer, resource.ScopesSupported)
}

// resourceMetadataURLFor is where the MCP server's protected resource
// metadata is: the WWW-Authenticate challenge's, else the well-known URL.
func resourceMetadataURLFor(ctx context.Context, endpoint *url.URL) string {
	if metadataURL := probeResourceMetadataURL(ctx, endpoint); metadataURL != "" {
		return metadataURL
	}
	return protectedResourceMetadataURL(endpoint)
}

// loadAuthorizationServer fetches issuer's metadata (RFC 8414, then OpenID
// Connect discovery) and keeps it only when it states exactly issuer.
// Identifiers are compared as strings, without normalization. Every fetched
// URL passes the same public-HTTPS check as DiscoverOAuth, and the result is
// checked by ValidateOAuthMetadata.
func loadAuthorizationServer(ctx context.Context, endpoint *url.URL, issuer string, scopes []string) (AuthorizationServer, error) {
	issuerURL, err := validateOAuthURL(ctx, issuer)
	if err != nil {
		return AuthorizationServer{}, fmt.Errorf("authorization server: %w", err)
	}
	var server struct {
		authorizationServerMetadata
		Issuer                string `json:"issuer"`
		ISSParameterSupported bool   `json:"authorization_response_iss_parameter_supported"`
	}
	var discoveryErr error
	for _, candidate := range authorizationMetadataURLs(issuerURL) {
		candidateURL, validateErr := validateOAuthURL(ctx, candidate)
		if validateErr != nil {
			discoveryErr = validateErr
			continue
		}
		server.authorizationServerMetadata = authorizationServerMetadata{}
		server.Issuer, server.ISSParameterSupported = "", false
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
		return AuthorizationServer{}, fmt.Errorf("load authorization server metadata: %w", discoveryErr)
	}
	if server.AuthorizationEndpoint == "" || server.TokenEndpoint == "" {
		return AuthorizationServer{}, errors.New("authorization server metadata is missing required endpoints")
	}
	if len(server.CodeChallengeMethodsSupported) > 0 && !containsString(server.CodeChallengeMethodsSupported, "S256") {
		return AuthorizationServer{}, errors.New("authorization server does not support PKCE S256")
	}
	metadata := OAuthMetadata{
		ResourceEndpoint:      endpoint.String(),
		AuthorizationEndpoint: server.AuthorizationEndpoint,
		TokenEndpoint:         server.TokenEndpoint,
		RegistrationEndpoint:  server.RegistrationEndpoint,
		Scopes:                append([]string(nil), scopes...),
		TokenAuthMethods:      append([]string(nil), server.TokenEndpointAuthMethodsSupport...),
	}
	if err := ValidateOAuthMetadata(ctx, metadata); err != nil {
		return AuthorizationServer{}, fmt.Errorf("authorization server endpoint: %w", err)
	}
	return AuthorizationServer{OAuthMetadata: metadata, Issuer: issuer, ISSParameterSupported: server.ISSParameterSupported}, nil
}
