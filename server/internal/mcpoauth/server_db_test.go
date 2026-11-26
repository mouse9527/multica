// Added by the mouse9527/multica fork: manual OAuth endpoints, resource and audience for MCP entries (M-295 stage 2).

package mcpoauth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// connectWith runs a connect for target through the fake server and returns
// the authorization URL it sent the browser to.
func connectWith(t *testing.T, fake *fakeOAuthServer, store *Store, f fixture, target Target) *url.URL {
	t.Helper()
	ctx := context.Background()
	flow := Flow{Store: store}
	authorizationURL, err := flow.Start(ctx, target, f.ownerID, testRedirectURI, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code, state := fake.authorize(t, authorizationURL)
	if _, err := flow.Complete(ctx, state, code, "", f.ownerID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	u, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Manual endpoints skip discovery entirely: no protected resource metadata,
// no authorization server metadata. Exchange and refresh go to the manual
// token endpoint.
func TestManualEndpointsConnectWithoutDiscovery(t *testing.T) {
	ctx := context.Background()
	fake, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
	fake.mu.Lock()
	fake.noResourceMetadata = true
	fake.mu.Unlock()
	// The metadata would name other endpoints; manual ones win.
	target.Server = ServerConfig{AuthorizationEndpoint: fake.srv.URL + "/authorize", TokenEndpoint: fake.srv.URL + "/token"}

	connectWith(t, fake, store, f, target)
	status, err := store.GetConnectionStatus(ctx, f.workspaceID, target.Ref, f.ownerID)
	if err != nil || status.Status != StatusConnected || status.TokenEndpoint != fake.srv.URL+"/token" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if got := status.ForEntry(Entry{URL: target.URL, Client: target.Client, Server: target.Server}); got.Status != StatusConnected {
		t.Fatalf("status for the same entry = %+v", got)
	}

	// The entry now names another token endpoint: the grant made for the old
	// one is not used.
	moved := target
	moved.Server.TokenEndpoint = fake.srv.URL + "/other-token"
	if got := status.ForEntry(Entry{URL: target.URL, Client: moved.Client, Server: moved.Server}); got.Status != StatusNeedsReconnect || got.LastError != ReasonServerChanged {
		t.Fatalf("status for a moved token endpoint = %+v", got)
	}
	if _, err := NewTokenSource(store, Refresh, nil).Credential(ctx, moved, f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("credential for a moved token endpoint: err = %v", err)
	}
}

// Manual endpoints are held to the same SSRF boundary as discovered ones.
func TestManualEndpointsRefuseNonPublicAddresses(t *testing.T) {
	_, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
	for _, endpoints := range []ServerConfig{
		{AuthorizationEndpoint: "https://127.0.0.1:1/authorize", TokenEndpoint: "https://127.0.0.1:1/token"},
		{AuthorizationEndpoint: "https://auth.example.com/authorize", TokenEndpoint: "https://localhost/token"},
	} {
		target.Server = endpoints
		if _, err := (Flow{Store: store}).Start(context.Background(), target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrEndpointNotAllowed) {
			t.Fatalf("endpoints %+v: err = %v, want ErrEndpointNotAllowed", endpoints, err)
		}
	}
}

// A named issuer works for servers without protected resource metadata,
// for a pre-registered client and for dynamic registration alike; metadata
// from the issuer must still state that issuer.
func TestIssuerWithoutResourceMetadata(t *testing.T) {
	t.Run("pre-registered", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
		fake.mu.Lock()
		fake.noResourceMetadata = true
		fake.mu.Unlock()
		connectWith(t, fake, store, f, target)
	})
	t.Run("dynamic registration", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		fake.mu.Lock()
		fake.noResourceMetadata = true
		fake.mu.Unlock()
		target.Client = ClientConfig{Issuer: fake.issuer()}
		connectWith(t, fake, store, f, target)
		if got := fake.registered(); len(got) != 1 {
			t.Fatalf("registered %v", got)
		}
	})
	t.Run("plain discovery still needs the metadata", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		fake.mu.Lock()
		fake.noResourceMetadata = true
		fake.mu.Unlock()
		if _, err := (Flow{Store: store}).Start(context.Background(), target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrDiscovery) {
			t.Fatalf("err = %v, want ErrDiscovery", err)
		}
	})
}

// A resource override is the resource indicator of the authorization, code
// and refresh requests; the audience goes on the authorization request.
func TestResourceAndAudience(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	fake := newFakeOAuthServer(t)
	fake.mu.Lock()
	fake.resource = "https://api.example.com/v1"
	fake.mu.Unlock()
	store := newTestStore(t, testPool(t), clock)
	f := newFixture()
	target := Target{
		WorkspaceID: f.workspaceID, Ref: f.target("gateway").Ref, URL: fake.srv.URL + "/mcp",
		Server: ServerConfig{Resource: "https://api.example.com/v1", Audience: "https://api.example.com"},
	}
	authorizationURL := connectWith(t, fake, store, f, target)
	if got := authorizationURL.Query().Get("audience"); got != "https://api.example.com" {
		t.Fatalf("audience = %q", got)
	}
	status, err := store.GetConnectionStatus(ctx, f.workspaceID, target.Ref, f.ownerID)
	if err != nil || status.Resource != "https://api.example.com/v1" {
		t.Fatalf("status = %+v, %v", status, err)
	}

	// The fake answers only requests with the overridden resource, so the
	// refresh succeeding shows it was sent there too.
	clock.Advance(fakeAccessLifetime*time.Second - time.Minute)
	credential, err := NewTokenSource(store, Refresh, nil).Credential(ctx, target, f.ownerID)
	if err != nil || credential.AccessToken.Reveal() != "at-2" {
		t.Fatalf("credential after refresh = %q, %v", credential.AccessToken.Reveal(), err)
	}

	// Without the override the grant is not for this entry any more.
	plain := target
	plain.Server.Resource = "https://api.example.com/v2"
	if _, err := NewTokenSource(store, Refresh, nil).Credential(ctx, plain, f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("other resource: err = %v", err)
	}
}

// Entries without the new fields send exactly what they sent before: no
// audience, the entry URL as the resource.
func TestNoServerSettingsSendWhatTheyDid(t *testing.T) {
	fake, store, f, target := newFlowHarness(t)
	u := connectWith(t, fake, store, f, target)
	q := u.Query()
	if q.Has("audience") || q.Get("resource") != target.URL {
		t.Fatalf("authorization URL = %s", u)
	}
}

// Discovered metadata must state the issuer it was fetched for, on the plain
// discovery path too (RFC 8414 §3.3): otherwise no client is registered and
// nothing is sent.
func TestDiscoveryRefusesMetadataOfAnotherIssuer(t *testing.T) {
	fake, store, f, target := newFlowHarness(t)
	fake.mu.Lock()
	fake.statedIssuer = "https://honest.example.com"
	fake.mu.Unlock()
	if _, err := (Flow{Store: store}).Start(context.Background(), target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("err = %v, want ErrDiscovery", err)
	}
	if got := fake.registered(); len(got) != 0 {
		t.Fatalf("registered %v with metadata of another issuer", got)
	}
}

// RFC 9207: the issuer is recorded with the connect, and the authorization
// response's iss is checked against it before the code is exchanged.
func TestAuthorizationResponseIssuer(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, fake *fakeOAuthServer, store *Store, f fixture, target Target) (code, state string) {
		t.Helper()
		authorizationURL, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		return fake.authorize(t, authorizationURL)
	}

	t.Run("advertised: absent or other iss is refused, same iss completes", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		fake.mu.Lock()
		fake.issSupported = true
		fake.mu.Unlock()
		flow := Flow{Store: store}

		code, state := start(t, fake, store, f, target)
		if issuer, required := issuerFromState(state); issuer != fake.issuer() || !required {
			t.Fatalf("state records %q, %v", issuer, required)
		}
		if _, err := flow.Complete(ctx, state, code, "", f.ownerID, nil); !errors.Is(err, ErrResponseIssuer) {
			t.Fatalf("absent iss: err = %v", err)
		}
		code, state = start(t, fake, store, f, target)
		if _, err := flow.Complete(ctx, state, code, "https://evil.example.com", f.ownerID, nil); !errors.Is(err, ErrResponseIssuer) {
			t.Fatalf("other iss: err = %v", err)
		}
		if n := fake.tokenRequestCount(); n != 0 {
			t.Fatalf("%d token requests after refused responses", n)
		}
		code, state = start(t, fake, store, f, target)
		if _, err := flow.Complete(ctx, state, code, fake.issuer(), f.ownerID, nil); err != nil {
			t.Fatalf("same iss: %v", err)
		}
	})

	t.Run("not advertised: absent iss proceeds, other iss is refused, even for an error response", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		flow := Flow{Store: store}
		code, state := start(t, fake, store, f, target)
		if _, err := flow.Complete(ctx, state, code, "", f.ownerID, nil); err != nil {
			t.Fatalf("absent iss: %v", err)
		}
		_, state = start(t, fake, store, f, target)
		if _, err := flow.Complete(ctx, state, "", "https://evil.example.com", f.ownerID, nil); !errors.Is(err, ErrResponseIssuer) {
			t.Fatalf("error response from another issuer: err = %v, want ErrResponseIssuer", err)
		}
	})

	t.Run("manual endpoints check a present iss against the entry's issuer", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
		target.Server = ServerConfig{AuthorizationEndpoint: fake.srv.URL + "/authorize", TokenEndpoint: fake.srv.URL + "/token"}
		flow := Flow{Store: store}
		code, state := start(t, fake, store, f, target)
		if issuer, required := issuerFromState(state); issuer != target.Client.Issuer || required {
			t.Fatalf("state records %q, %v", issuer, required)
		}
		if _, err := flow.Complete(ctx, state, code, "https://evil.example.com", f.ownerID, nil); !errors.Is(err, ErrResponseIssuer) {
			t.Fatalf("other iss: err = %v", err)
		}
	})

	t.Run("a state whose issuer part was changed is unknown", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		fake.mu.Lock()
		fake.issSupported = true
		fake.mu.Unlock()
		code, state := start(t, fake, store, f, target)
		random, _, _ := strings.Cut(state, ".")
		forged := stateWithIssuer(random, "https://evil.example.com", false)
		if _, err := (Flow{Store: store}).Complete(ctx, forged, code, "https://evil.example.com", f.ownerID, nil); !errors.Is(err, ErrStateInvalid) {
			t.Fatalf("forged state: err = %v, want ErrStateInvalid", err)
		}
	})
}

// Stage 3 (M-295): the extra parameters go on the authorization request
// only, never on the code exchange or refresh.
func TestAuthorizationParamsOnlyOnTheAuthorizationRequest(t *testing.T) {
	fake, store, f, target := newFlowHarness(t)
	target.Server.Params = AuthorizationParams{Prompt: "consent", AccessType: "offline"}
	u := connectWith(t, fake, store, f, target)
	if q := u.Query(); q.Get("prompt") != "consent" || q.Get("access_type") != "offline" || q.Get("client_id") != "client-xyz" {
		t.Fatalf("authorization URL = %s", u)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, form := range fake.tokenRequests {
		if form.Has("prompt") || form.Has("access_type") {
			t.Fatalf("token request carries authorization parameters: %v", form.Encode())
		}
	}
}

// Stage 2 second-review Nit 1: with a named issuer the RFC 9728 document is
// optional, but one that is published for another resource is an error, as
// on the plain discovery path.
func TestIssuerRefusesResourceMetadataForAnotherResource(t *testing.T) {
	fake, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
	fake.mu.Lock()
	fake.statedResource = "https://elsewhere.example.com/mcp"
	fake.mu.Unlock()
	_, err := (Flow{Store: store}).Start(context.Background(), target, f.ownerID, testRedirectURI, "")
	if !errors.Is(err, ErrDiscovery) || !strings.Contains(err.Error(), "does not match the MCP endpoint") {
		t.Fatalf("err = %v, want ErrDiscovery for a mismatched resource", err)
	}
}
