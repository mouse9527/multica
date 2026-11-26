// Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).

package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testClient is a pre-registered client for tests that store a secret
// without connecting.
var testClient = ClientConfig{ID: "client-1", Issuer: "https://auth.example.com"}

// preregisteredHarness is newFlowHarness with an authorization server that
// offers no dynamic registration and knows only client "pre-1".
func preregisteredHarness(t *testing.T, secret string, methods []string, client ClientConfig) (*fakeOAuthServer, *Store, fixture, Target) {
	t.Helper()
	fake, store, f, target := newFlowHarness(t)
	fake.preregister("pre-1", secret, methods)
	if client.Issuer == "" {
		client.Issuer = fake.issuer()
	}
	target.Client = client
	return fake, store, f, target
}

func connect(t *testing.T, fake *fakeOAuthServer, store *Store, f fixture, target Target) {
	t.Helper()
	ctx := context.Background()
	flow := Flow{Store: store}
	authorizationURL, err := flow.Start(ctx, target, f.ownerID, testRedirectURI, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code, state := fake.authorize(t, authorizationURL)
	if _, err := flow.Complete(ctx, state, code, f.ownerID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// A pre-registered client skips dynamic registration and authenticates at
// the token endpoint the way it is configured to, for the code exchange and
// for every refresh.
func TestPreregisteredClientConnectsAndRefreshes(t *testing.T) {
	for _, c := range []struct {
		name     string
		secret   string
		methods  []string
		explicit string
		want     string
	}{
		{"basic by default", "s3cret", nil, "", "basic"},
		{"post when only post is offered", "s3cret", []string{AuthMethodClientSecretPost}, "", "post"},
		{"post when the entry asks", "s3cret", []string{AuthMethodClientSecretBasic, AuthMethodClientSecretPost}, AuthMethodClientSecretPost, "post"},
		{"public client", "", []string{AuthMethodNone}, "", "none"},
		{"public client by choice", "s3cret", nil, AuthMethodNone, "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			serverSecret := c.secret
			if c.explicit == AuthMethodNone {
				serverSecret = ""
			}
			clock := newClock()
			fake := newFakeOAuthServer(t)
			fake.preregister("pre-1", serverSecret, c.methods)
			store := newTestStore(t, testPool(t), clock)
			f := newFixture()
			target := Target{
				WorkspaceID: f.workspaceID, Ref: WorkspaceRef(uuid.NewString()), URL: fake.srv.URL + "/mcp",
				Client: ClientConfig{ID: "pre-1", Issuer: fake.issuer(), TokenEndpointAuthMethod: c.explicit},
			}
			if c.secret != "" {
				if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, target.Client, NewSecret(c.secret)); err != nil {
					t.Fatal(err)
				}
			}
			connect(t, fake, store, f, target)
			if got := fake.registered(); len(got) != 0 {
				t.Fatalf("registered %v; a pre-registered client must not register", got)
			}

			clock.Advance(fakeAccessLifetime*time.Second - time.Minute)
			credential, err := NewTokenSource(store, Refresh, nil).Credential(ctx, target, f.ownerID)
			if err != nil || credential.AccessToken.Reveal() != "at-2" {
				t.Fatalf("credential after refresh = %q, %v", credential.AccessToken.Reveal(), err)
			}
			if got := fake.tokenAuthMethods(); len(got) != 2 || got[0] != c.want || got[1] != c.want {
				t.Fatalf("token requests authenticated as %v, want %s twice", got, c.want)
			}
		})
	}
}

func TestPreregisteredClientRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("a secret method without a secret", func(t *testing.T) {
		_, store, f, target := preregisteredHarness(t, "s3cret", nil, ClientConfig{ID: "pre-1", TokenEndpointAuthMethod: AuthMethodClientSecretBasic})
		if _, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrClientSecretMissing) {
			t.Fatalf("err = %v, want ErrClientSecretMissing", err)
		}
	})

	t.Run("an issuer the MCP server does not name", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
		target.Client.Issuer = fake.srv.URL + "/other"
		if _, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrIssuerMismatch) {
			t.Fatalf("err = %v, want ErrIssuerMismatch", err)
		}
		if n := fake.tokenRequestCount(); n != 0 {
			t.Fatalf("%d token requests", n)
		}
	})

	t.Run("metadata that states another issuer", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "s3cret", nil, ClientConfig{ID: "pre-1"})
		if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, target.Client, NewSecret("s3cret")); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		fake.statedIssuer = "https://honest.example.com"
		fake.mu.Unlock()
		if _, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrIssuerMismatch) {
			t.Fatalf("err = %v, want ErrIssuerMismatch", err)
		}
	})

	// The review case (M-295 R1): a secret set while the entry named issuer
	// A is never sent once the entry names issuer B, even when B's metadata
	// checks out; the same for another client id.
	t.Run("a secret set for another issuer or client id", func(t *testing.T) {
		for _, setFor := range []ClientConfig{
			{ID: "pre-1", Issuer: "https://old-auth.example.com"},
			{ID: "old-client", Issuer: ""},
		} {
			fake, store, f, target := preregisteredHarness(t, "s3cret", nil, ClientConfig{ID: "pre-1"})
			if setFor.Issuer == "" {
				setFor.Issuer = target.Client.Issuer
			}
			if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, setFor, NewSecret("secret-of-the-old-client")); err != nil {
				t.Fatal(err)
			}
			if _, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrClientSecretStale) {
				t.Fatalf("set for %+v: err = %v, want ErrClientSecretStale", setFor, err)
			}
			if n := fake.tokenRequestCount(); n != 0 {
				t.Fatalf("set for %+v: %d token requests", setFor, n)
			}
			// Set again for the client the entry names now, it connects.
			if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, target.Client, NewSecret("s3cret")); err != nil {
				t.Fatal(err)
			}
			connect(t, fake, store, f, target)
		}
	})

	t.Run("no dynamic registration and no client", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		fake.preregister("pre-1", "", nil)
		if _, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrNeedsPreregistered) {
			t.Fatalf("err = %v, want ErrNeedsPreregistered", err)
		}
	})
}

// The secret is sealed, never stored or returned in plaintext, and only its
// presence can be listed.
func TestClientSecretIsSealedAndWriteOnly(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f := newFixture()
	ref := f.target("gateway").Ref

	if err := store.SetClientSecret(ctx, f.workspaceID, ref, f.ownerID, testClient, NewSecret("sk-plain-secret")); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT client_secret_ciphertext FROM mcp_oauth_client WHERE server_ref = $1`, ref).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-plain-secret")) {
		t.Fatal("client secret stored in plaintext")
	}
	if got, err := store.clientSecret(ctx, f.workspaceID, ref, testClient); err != nil || got.Reveal() != "sk-plain-secret" {
		t.Fatalf("opened %v, %v", got, err)
	}
	// Bound to the client it was set for: another client id or issuer
	// never gets it.
	for _, other := range []ClientConfig{
		{ID: "another-client", Issuer: testClient.Issuer},
		{ID: testClient.ID, Issuer: "https://other-auth.example.com"},
	} {
		if got, err := store.clientSecret(ctx, f.workspaceID, ref, other); !errors.Is(err, ErrClientSecretStale) || !got.Empty() {
			t.Fatalf("secret for %+v: %v, %v", other, got, err)
		}
	}
	if err := store.SetClientSecret(ctx, f.workspaceID, ref, f.ownerID, ClientConfig{}, NewSecret("s")); !errors.Is(err, ErrNotPreregistered) {
		t.Fatalf("secret without a client: err = %v", err)
	}
	if raw, _ := json.Marshal(map[string]any{"secret": NewSecret("sk-plain-secret")}); bytes.Contains(raw, []byte("sk-plain")) {
		t.Fatal("a Secret marshals its value")
	}
	// Another workspace sees nothing.
	if got, err := store.clientSecret(ctx, uuid.NewString(), ref, testClient); err != nil || !got.Empty() {
		t.Fatalf("another workspace read %v, %v", got, err)
	}
	bindings, err := ClientSecretBindings(ctx, pool, f.workspaceID, []string{ref, f.target("other").Ref})
	if err != nil || bindings[ref] != testClient {
		t.Fatalf("bindings = %v, %v", bindings, err)
	}
	if _, found := bindings[f.target("other").Ref]; found {
		t.Fatalf("bindings = %v", bindings)
	}
	entry := Entry{URL: testServerURL, Client: testClient}
	moved := Entry{URL: testServerURL, Client: ClientConfig{ID: testClient.ID, Issuer: "https://other-auth.example.com"}}
	if !SecretSetFor(bindings[ref], true, entry) || SecretSetFor(bindings[ref], true, moved) || SecretSetFor(ClientConfig{}, false, entry) {
		t.Fatal("SecretSetFor does not follow the binding")
	}
	if err := store.SetClientSecret(ctx, f.workspaceID, ref, f.ownerID, testClient, NewSecret("bad\nsecret")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid secret: err = %v", err)
	}

	removed, err := store.DeleteClientSecret(ctx, f.workspaceID, ref)
	if err != nil || !removed {
		t.Fatalf("delete = %v, %v", removed, err)
	}
	if removed, err := store.DeleteClientSecret(ctx, f.workspaceID, ref); err != nil || removed {
		t.Fatalf("second delete = %v, %v", removed, err)
	}
	if rowCount(t, pool, "mcp_oauth_client", ref) != 0 {
		t.Fatal("secret left behind")
	}
}

// Replacing or removing the secret, or the entry naming another client,
// takes the grant made with the old client out of use.
func TestChangedClientNeedsReconnect(t *testing.T) {
	ctx := context.Background()

	t.Run("new secret", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "s3cret", nil, ClientConfig{ID: "pre-1"})
		if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, target.Client, NewSecret("s3cret")); err != nil {
			t.Fatal(err)
		}
		connect(t, fake, store, f, target)
		// A connect in flight carries the old secret and is cancelled too.
		if _, err := (Flow{Store: store}).Start(ctx, target, f.ownerID, testRedirectURI, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, target.Client, NewSecret("rotated")); err != nil {
			t.Fatal(err)
		}
		status, err := store.GetConnectionStatus(ctx, f.workspaceID, target.Ref, f.ownerID)
		if err != nil || status.Status != StatusNeedsReconnect || status.LastError != ReasonClientChanged {
			t.Fatalf("status = %+v, %v", status, err)
		}
		if _, err := NewTokenSource(store, Refresh, nil).Credential(ctx, target, f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
			t.Fatalf("credential: err = %v", err)
		}
		var pending int
		if err := store.db.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_state WHERE server_ref = $1`, target.Ref).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("%d connects in flight survived the new secret (%v)", pending, err)
		}
	})

	t.Run("secret removed", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "s3cret", nil, ClientConfig{ID: "pre-1"})
		if err := store.SetClientSecret(ctx, f.workspaceID, target.Ref, f.ownerID, target.Client, NewSecret("s3cret")); err != nil {
			t.Fatal(err)
		}
		connect(t, fake, store, f, target)
		if _, err := store.DeleteClientSecret(ctx, f.workspaceID, target.Ref); err != nil {
			t.Fatal(err)
		}
		status, err := store.GetConnectionStatus(ctx, f.workspaceID, target.Ref, f.ownerID)
		if err != nil || status.Status != StatusNeedsReconnect || status.LastError != ReasonClientChanged {
			t.Fatalf("status = %+v, %v", status, err)
		}
	})

	t.Run("another client id or method", func(t *testing.T) {
		fake, store, f, target := preregisteredHarness(t, "", nil, ClientConfig{ID: "pre-1"})
		connect(t, fake, store, f, target)
		source := NewTokenSource(store, Refresh, nil)
		if _, err := source.Credential(ctx, target, f.ownerID); err != nil {
			t.Fatalf("same client: %v", err)
		}
		pinned := target
		pinned.Client.TokenEndpointAuthMethod = AuthMethodNone
		if _, err := source.Credential(ctx, pinned, f.ownerID); err != nil {
			t.Fatalf("the method it already used: %v", err)
		}
		moved := target
		moved.Client.ID = "pre-2"
		if _, err := source.Credential(ctx, moved, f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
			t.Fatalf("other client id: err = %v", err)
		}
		status, err := store.GetConnectionStatus(ctx, f.workspaceID, target.Ref, f.ownerID)
		if err != nil || status.Status != StatusNeedsReconnect || status.LastError != ReasonClientChanged {
			t.Fatalf("status = %+v, %v", status, err)
		}
	})

	t.Run("dynamic grant, entry now pre-registered", func(t *testing.T) {
		fake, store, f, target := newFlowHarness(t)
		connect(t, fake, store, f, target)
		target.Client = ClientConfig{ID: "pre-1", Issuer: fake.issuer()}
		if _, err := NewTokenSource(store, Refresh, nil).Credential(ctx, target, f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
			t.Fatalf("err = %v, want ErrNeedsReconnect", err)
		}
	})

	t.Run("marked on save", func(t *testing.T) {
		pool := testPool(t)
		store := newTestStore(t, pool, newClock())
		f := newFixture()
		ref := f.target("gateway").Ref
		if err := store.SaveGrant(ctx, f.grant(ref, "at", "rt", 3600)); err != nil {
			t.Fatal(err)
		}
		if err := MarkEntryNeedsReconnect(ctx, pool, f.workspaceID, ref, ReasonClientChanged, time.Now()); err != nil {
			t.Fatal(err)
		}
		status, err := store.GetConnectionStatus(ctx, f.workspaceID, ref, f.ownerID)
		if err != nil || status.Status != StatusNeedsReconnect || status.LastError != ReasonClientChanged {
			t.Fatalf("status = %+v, %v", status, err)
		}
		var access []byte
		if err := pool.QueryRow(ctx, `SELECT access_token_ciphertext FROM mcp_oauth_connection WHERE server_ref = $1`, ref).Scan(&access); err != nil || access != nil {
			t.Fatalf("token kept after the client changed: %v", err)
		}
	})
}

// A completion is refused when the entry names another client than the
// connect started with.
func TestPendingMatchesEntry(t *testing.T) {
	p := PendingAuthorization{ServerURL: "https://mcp.example.com/mcp", Client: Client{ID: "pre-1", TokenEndpointAuthMethod: AuthMethodClientSecretBasic}}
	entry := Entry{URL: "https://mcp.example.com/mcp", Client: ClientConfig{ID: "pre-1", Issuer: "https://a.example"}}
	if !p.MatchesEntry(entry) {
		t.Fatal("same entry refused")
	}
	other := entry
	other.Client.ID = "pre-2"
	if p.MatchesEntry(other) {
		t.Fatal("other client accepted")
	}
	moved := entry
	moved.URL = "https://elsewhere.example.com/mcp"
	if p.MatchesEntry(moved) {
		t.Fatal("other URL accepted")
	}
}

// The secret goes with its entry, an agent's removed entries and its
// workspace.
func TestClientSecretCleanup(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f := newFixture()
	set := func(ref string) {
		t.Helper()
		if err := store.SetClientSecret(ctx, f.workspaceID, ref, f.ownerID, testClient, NewSecret("s")); err != nil {
			t.Fatal(err)
		}
	}

	wsRef := WorkspaceRef(uuid.NewString())
	set(wsRef)
	if err := DeleteEntryData(ctx, pool, f.workspaceID, wsRef); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, pool, "mcp_oauth_client", wsRef) != 0 {
		t.Fatal("entry delete left its secret")
	}

	kept, renamed := f.target("kept").Ref, f.target("renamed").Ref
	set(kept)
	set(renamed)
	if err := DeleteAgentEntryDataExcept(ctx, pool, f.workspaceID, f.agentID, []string{kept}); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, pool, "mcp_oauth_client", kept) != 1 || rowCount(t, pool, "mcp_oauth_client", renamed) != 0 {
		t.Fatal("agent cleanup kept the wrong secrets")
	}

	if err := ForgetClientSecret(ctx, pool, f.workspaceID, kept); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, pool, "mcp_oauth_client", kept) != 0 {
		t.Fatal("ForgetClientSecret left the secret")
	}

	set(kept)
	if err := DeleteWorkspaceData(ctx, pool, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, pool, "mcp_oauth_client", kept) != 0 {
		t.Fatal("workspace delete left a secret")
	}
}
