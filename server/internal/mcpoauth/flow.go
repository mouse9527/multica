// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: connect an MCP entry by ref (M-269).
// Modified for the mouse9527/multica fork: rejected refreshes via remotemcp.OAuthError (M-277).

package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// CallbackPath is the web app page the authorization server sends the
// browser back to. The page posts the code and state to the API with the
// user's own session, which is what binds a grant to the person who started
// the connect: a callback that trusted the state alone would let someone who
// started a connect on their own agent trick another user into authorizing
// it, attaching the victim's identity to the attacker's agent. "/oauth" is a
// reserved workspace slug.
const CallbackPath = "/oauth/mcp"

// RedirectURI is CallbackPath on the app origin. The MCP authorization spec
// requires an HTTPS redirect URI (or loopback), so a plain-HTTP app origin is
// refused unless it is loopback.
func RedirectURI(appURL string) (string, error) {
	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	u, err := url.Parse(appURL)
	if appURL == "" || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("the app URL is not configured")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return "", errors.New("the app URL must be HTTPS for OAuth redirects")
	}
	return appURL + CallbackPath, nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Errors from the connect flow. Their text is safe to show the person who
// started the connect; the wrapped cause stays out of responses.
var (
	ErrDiscovery           = errors.New("could not discover the server's OAuth configuration")
	ErrRegistration        = errors.New("the authorization server did not register this client")
	ErrNeedsPreregistered  = errors.New("the authorization server offers no dynamic client registration; pre-registered clients are not supported yet")
	ErrUnsupportedClient   = errors.New("the authorization server registered a client this server cannot use")
	ErrExchange            = errors.New("the authorization server did not issue a token")
	ErrAuthorizationDenied = errors.New("the authorization was denied or cancelled")
)

// Flow runs the authorization code flow with PKCE (S256), state and the
// RFC 8707 resource parameter. Every protocol step is pkg/remotemcp's, which
// only reaches public HTTPS endpoints (plus the operator's exact origins in
// AllowedOriginsEnv) and follows no redirects.
type Flow struct {
	Store *Store
}

// Start begins connecting userID to the target entry: discovery (RFC 9728,
// then RFC 8414 or OIDC), dynamic client registration (RFC 7591), a PKCE
// verifier and a one-time state. It returns the URL to send the browser to.
// The pending authorization records target.URL, and the grant it ends in is
// bound to it.
func (f Flow) Start(ctx context.Context, target Target, userID, redirectURI, returnTo string) (string, error) {
	ctx = withAllowedOrigins(ctx)
	metadata, err := remotemcp.DiscoverOAuth(ctx, target.URL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDiscovery, err)
	}
	if metadata.RegistrationEndpoint == "" {
		return "", ErrNeedsPreregistered
	}
	registration, err := remotemcp.RegisterOAuthClient(ctx, metadata, redirectURI)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRegistration, err)
	}
	method, err := normalizeAuthMethod(registration.TokenEndpointAuthMethod)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnsupportedClient, err)
	}
	verifier, err := NewVerifier()
	if err != nil {
		return "", err
	}
	state, err := f.Store.CreatePendingAuthorization(ctx, PendingAuthorization{
		WorkspaceID:   target.WorkspaceID,
		ServerRef:     target.Ref,
		ServerURL:     target.URL,
		UserID:        userID,
		Resource:      metadata.ResourceEndpoint,
		TokenEndpoint: metadata.TokenEndpoint,
		Client:        Client{ID: registration.ClientID, Secret: NewSecret(registration.ClientSecret), TokenEndpointAuthMethod: method},
		RedirectURI:   redirectURI,
		Verifier:      verifier,
		ReturnTo:      returnTo,
	})
	if err != nil {
		return "", err
	}
	return remotemcp.BuildAuthorizationURL(metadata, registration, redirectURI, state, verifier.Reveal(), target.Scope)
}

// Complete finishes a connect: it takes the pending authorization the state
// belongs to (only userID's, only once), lets check refuse it, exchanges the
// code with the PKCE verifier and stores the grant. check runs before the
// code is exchanged, so a refused completion never obtains a token.
func (f Flow) Complete(ctx context.Context, state, code, userID string, check func(PendingAuthorization) error) (PendingAuthorization, error) {
	pending, err := f.Store.ConsumePendingAuthorization(ctx, state, userID)
	if err != nil {
		return PendingAuthorization{}, err
	}
	if check != nil {
		if err := check(pending); err != nil {
			return pending, err
		}
	}
	if strings.TrimSpace(code) == "" {
		return pending, ErrAuthorizationDenied
	}
	token, err := remotemcp.ExchangeOAuthCode(withAllowedOrigins(ctx), pending.TokenEndpoint, pending.Resource, code,
		pending.RedirectURI, pending.Verifier.Reveal(), pending.Client.registration())
	if err != nil {
		return pending, fmt.Errorf("%w: %w", ErrExchange, err)
	}
	err = f.Store.SaveGrant(ctx, Grant{
		WorkspaceID:   pending.WorkspaceID,
		ServerRef:     pending.ServerRef,
		ServerURL:     pending.ServerURL,
		UserID:        pending.UserID,
		Resource:      pending.Resource,
		TokenEndpoint: pending.TokenEndpoint,
		Client:        pending.Client,
		AccessToken:   NewSecret(token.AccessToken),
		RefreshToken:  NewSecret(token.RefreshToken),
		ExpiresIn:     token.ExpiresIn,
		Scope:         token.Scope,
	})
	return pending, err
}

// Refresh is the Refresher TokenSource uses in production: the refresh_token
// grant through pkg/remotemcp. A token endpoint answer that
// remotemcp.OAuthError reports as Rejected (invalid_grant, invalid_client,
// unauthorized_client, or a bare 400/401) means the grant itself is gone and
// is reported as ErrGrantRejected. Anything else — 5xx, 429, another OAuth
// error code, a network error — is transient.
func Refresh(ctx context.Context, req RefreshRequest) (remotemcp.OAuthTokenResponse, error) {
	response, err := remotemcp.RefreshOAuthToken(withAllowedOrigins(ctx), req.TokenEndpoint, req.Resource, req.RefreshToken.Reveal(), req.Client)
	if err != nil {
		var oauthErr *remotemcp.OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Rejected() {
			return remotemcp.OAuthTokenResponse{}, fmt.Errorf("%w: %w", ErrGrantRejected, err)
		}
		return remotemcp.OAuthTokenResponse{}, err
	}
	return response, nil
}
