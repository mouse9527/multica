// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	return testutil.IsolatedDB(t, dsn)
}

// testClock is a settable clock shared by the store and the test.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func newTestStore(t *testing.T, pool *pgxpool.Pool, clock *testClock) *Store {
	t.Helper()
	store, err := NewStore(pool, newBox(t))
	if err != nil {
		t.Fatal(err)
	}
	store.now = clock.Now
	return store
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)}
}

type fixture struct {
	workspaceID string
	agentID     string
	ownerID     string
}

func newFixture() fixture {
	return fixture{workspaceID: uuid.NewString(), agentID: uuid.NewString(), ownerID: uuid.NewString()}
}

func (f fixture) server(t *testing.T, store *Store, name string, headers map[string]string) Server {
	t.Helper()
	server, err := store.CreateServer(context.Background(), ServerInput{
		WorkspaceID: f.workspaceID, AgentID: f.agentID, CreatedBy: f.ownerID,
		Name: name, URL: "https://mcp.example.com/" + name, ExtraHeaders: headers,
	})
	if err != nil {
		t.Fatalf("create server %s: %v", name, err)
	}
	return server
}

func (f fixture) grant(serverID, access, refresh string, expiresIn int64) Grant {
	return Grant{
		WorkspaceID: f.workspaceID, ServerID: serverID, UserID: f.ownerID,
		Resource: "https://mcp.example.com/gateway", TokenEndpoint: "https://auth.example.com/token",
		Client:      Client{ID: "client-1", Secret: NewSecret("client-secret-1"), TokenEndpointAuthMethod: "client_secret_post"},
		AccessToken: NewSecret(access), RefreshToken: NewSecret(refresh), ExpiresIn: expiresIn, Scope: "mcp",
	}
}

func TestServerLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f := newFixture()

	gateway := f.server(t, store, "gateway", map[string]string{"x-gateway-key": "gw-key-value"})
	if !gateway.HasExtraHeaders || gateway.Name != "gateway" || gateway.AgentID != f.agentID {
		t.Fatalf("created server = %+v", gateway)
	}
	plain := f.server(t, store, "plain", nil)
	if plain.HasExtraHeaders {
		t.Fatal("a server without headers reports headers")
	}

	// The name is unique per agent, not per workspace.
	if _, err := store.CreateServer(ctx, ServerInput{
		WorkspaceID: f.workspaceID, AgentID: f.agentID, CreatedBy: f.ownerID, Name: "gateway", URL: "https://other.example.com/mcp",
	}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: err = %v", err)
	}
	if _, err := store.CreateServer(ctx, ServerInput{
		WorkspaceID: f.workspaceID, AgentID: uuid.NewString(), CreatedBy: f.ownerID, Name: "gateway", URL: "https://other.example.com/mcp",
	}); err != nil {
		t.Fatalf("same name on another agent: %v", err)
	}

	// Header values are sealed.
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT extra_headers_ciphertext FROM mcp_oauth_server WHERE id = $1::uuid`, gateway.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("gw-key-value")) {
		t.Fatal("extra header value stored in plaintext")
	}

	listed, err := store.ListServersForAgent(ctx, f.workspaceID, f.agentID)
	if err != nil || len(listed) != 2 || listed[0].Name != "gateway" || listed[1].Name != "plain" {
		t.Fatalf("list = %+v, err = %v", listed, err)
	}
	// Another workspace does not see it.
	if _, err := store.GetServer(ctx, uuid.NewString(), gateway.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace get: err = %v", err)
	}

	// Deleting a server takes its connections and pending authorizations.
	if err := store.SaveGrant(ctx, f.grant(gateway.ID, "at-1", "rt-1", 3600)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePendingAuthorization(ctx, f.pending(gateway.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteServer(ctx, f.workspaceID, gateway.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"mcp_oauth_server", "mcp_oauth_connection", "mcp_oauth_state"} {
		column := "server_id"
		if table == "mcp_oauth_server" {
			column = "id"
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+column+` = $1::uuid`, gateway.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s keeps %d rows of the deleted server", table, n)
		}
	}
	if err := store.DeleteServer(ctx, f.workspaceID, gateway.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: err = %v", err)
	}
}

func (f fixture) pending(serverID string) PendingAuthorization {
	return PendingAuthorization{
		WorkspaceID: f.workspaceID, ServerID: serverID, UserID: f.ownerID,
		Resource: "https://mcp.example.com/gateway", TokenEndpoint: "https://auth.example.com/token",
		Client:      Client{ID: "client-1", Secret: NewSecret("client-secret-1"), TokenEndpointAuthMethod: "client_secret_basic"},
		RedirectURI: "https://multica.example.com/api/mcp-oauth/callback",
		Verifier:    NewSecret("verifier-value-0123456789-0123456789-0123456"),
		ReturnTo:    "/settings",
	}
}

func TestPendingAuthorizationIsSingleUse(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	clock := newClock()
	store := newTestStore(t, pool, clock)
	f := newFixture()
	server := f.server(t, store, "gateway", nil)

	state, err := store.CreatePendingAuthorization(ctx, f.pending(server.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(state) < 43 {
		t.Fatalf("state %q is too short", state)
	}
	// Only the hash is stored, and the verifier and client secret are sealed.
	var hash, verifier, clientSecret []byte
	if err := pool.QueryRow(ctx, `SELECT state_hash, verifier_ciphertext, client_secret_ciphertext FROM mcp_oauth_state`).Scan(&hash, &verifier, &clientSecret); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(hash, []byte(state)) || bytes.Contains(verifier, []byte("verifier-value")) || bytes.Contains(clientSecret, []byte("client-secret-1")) {
		t.Fatal("state, verifier or client secret stored in plaintext")
	}

	got, err := store.ConsumePendingAuthorization(ctx, state, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	want := f.pending(server.ID)
	if got.ServerID != server.ID || got.Verifier.Reveal() != want.Verifier.Reveal() || got.Client.Secret.Reveal() != "client-secret-1" ||
		got.Client.TokenEndpointAuthMethod != "client_secret_basic" || got.RedirectURI != want.RedirectURI || got.ReturnTo != "/settings" {
		t.Fatalf("consumed = %+v", got)
	}
	if _, err := store.ConsumePendingAuthorization(ctx, state, f.ownerID); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("second use: err = %v", err)
	}

	// Another user's callback cannot use it, and the attempt burns it.
	state, err = store.CreatePendingAuthorization(ctx, f.pending(server.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumePendingAuthorization(ctx, state, uuid.NewString()); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("other user: err = %v", err)
	}
	if _, err := store.ConsumePendingAuthorization(ctx, state, f.ownerID); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("after another user's attempt: err = %v", err)
	}

	// An expired state fails, and the next start clears it.
	state, err = store.CreatePendingAuthorization(ctx, f.pending(server.ID))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(PendingTTL + time.Second)
	if _, err := store.ConsumePendingAuthorization(ctx, state, f.ownerID); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("expired: err = %v", err)
	}
	if _, err := store.ConsumePendingAuthorization(ctx, "", f.ownerID); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("empty state: err = %v", err)
	}
	stale, err := store.CreatePendingAuthorization(ctx, f.pending(server.ID))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(PendingTTL + time.Second)
	if _, err := store.CreatePendingAuthorization(ctx, f.pending(server.ID)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_state WHERE state_hash = $1`, stateHash(stale)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("an expired state was not cleared")
	}
}

func TestGrantIsSealedAndReplacedOnReconnect(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	clock := newClock()
	store := newTestStore(t, pool, clock)
	f := newFixture()
	server := f.server(t, store, "gateway", nil)

	if _, err := store.GetConnectionStatus(ctx, f.workspaceID, server.ID, f.ownerID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("before connect: err = %v", err)
	}
	if err := store.SaveGrant(ctx, f.grant(server.ID, "access-token-one", "refresh-token-one", 3600)); err != nil {
		t.Fatal(err)
	}
	var access, refresh, clientSecret []byte
	if err := pool.QueryRow(ctx, `
		SELECT access_token_ciphertext, refresh_token_ciphertext, client_secret_ciphertext
		FROM mcp_oauth_connection WHERE server_id = $1::uuid`, server.ID).Scan(&access, &refresh, &clientSecret); err != nil {
		t.Fatal(err)
	}
	for name, sealed := range map[string][]byte{"access": access, "refresh": refresh, "client secret": clientSecret} {
		if len(sealed) == 0 || bytes.Contains(sealed, []byte("token-one")) || bytes.Contains(sealed, []byte("client-secret-1")) {
			t.Errorf("%s token is missing or stored in plaintext", name)
		}
	}
	status, err := store.GetConnectionStatus(ctx, f.workspaceID, server.ID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusConnected || status.ExpiresAt == nil || !status.ExpiresAt.Equal(clock.Now().Add(time.Hour)) || status.GrantedScope != "mcp" {
		t.Fatalf("status = %+v", status)
	}

	// A token endpoint that gives no lifetime leaves expires_at empty.
	clock.Advance(time.Minute)
	if err := store.SaveGrant(ctx, f.grant(server.ID, "access-token-two", "", 0)); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetConnectionStatus(ctx, f.workspaceID, server.ID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ExpiresAt != nil || !status.ConnectedAt.Equal(clock.Now()) {
		t.Fatalf("after reconnect: %+v", status)
	}
	statuses, err := store.ListConnectionStatusesForAgent(ctx, f.workspaceID, f.agentID)
	if err != nil || len(statuses) != 1 || statuses[0].ServerID != server.ID {
		t.Fatalf("list statuses = %+v, err = %v", statuses, err)
	}

	if err := store.DeleteConnection(ctx, f.workspaceID, server.ID, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteConnection(ctx, f.workspaceID, server.ID, f.ownerID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("second disconnect: err = %v", err)
	}
}

func TestDeleteWorkspaceData(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f, other := newFixture(), newFixture()
	for _, fx := range []fixture{f, other} {
		server := fx.server(t, store, "gateway", nil)
		if err := store.SaveGrant(ctx, fx.grant(server.ID, "at", "rt", 3600)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreatePendingAuthorization(ctx, fx.pending(server.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteWorkspaceData(ctx, pool, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"mcp_oauth_server", "mcp_oauth_connection", "mcp_oauth_state"} {
		var mine, theirs int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE workspace_id = $1::uuid), count(*) FILTER (WHERE workspace_id = $2::uuid)
			FROM `+table, f.workspaceID, other.workspaceID).Scan(&mine, &theirs); err != nil {
			t.Fatal(err)
		}
		if mine != 0 || theirs != 1 {
			t.Errorf("%s: %d rows left in the deleted workspace, %d in the other (want 0 and 1)", table, mine, theirs)
		}
	}
}
