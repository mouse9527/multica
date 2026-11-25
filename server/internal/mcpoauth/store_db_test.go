// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: connections keyed by entry ref (M-269).

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

const testServerURL = "https://mcp.example.com/gateway"

// target is the fixture agent's own entry under name.
func (f fixture) target(name string) Target {
	return Target{WorkspaceID: f.workspaceID, Ref: AgentRef(f.agentID, name), URL: testServerURL}
}

func (f fixture) grant(ref, access, refresh string, expiresIn int64) Grant {
	return Grant{
		WorkspaceID: f.workspaceID, ServerRef: ref, ServerURL: testServerURL, UserID: f.ownerID,
		Resource: testServerURL, TokenEndpoint: "https://auth.example.com/token",
		Client:      Client{ID: "client-1", Secret: NewSecret("client-secret-1"), TokenEndpointAuthMethod: "client_secret_post"},
		AccessToken: NewSecret(access), RefreshToken: NewSecret(refresh), ExpiresIn: expiresIn, Scope: "mcp",
	}
}

func (f fixture) pending(ref string) PendingAuthorization {
	return PendingAuthorization{
		WorkspaceID: f.workspaceID, ServerRef: ref, ServerURL: testServerURL, UserID: f.ownerID,
		Resource: testServerURL, TokenEndpoint: "https://auth.example.com/token",
		Client:      Client{ID: "client-1", Secret: NewSecret("client-secret-1"), TokenEndpointAuthMethod: "client_secret_basic"},
		RedirectURI: "https://multica.example.com/api/mcp-oauth/callback",
		Verifier:    NewSecret("verifier-value-0123456789-0123456789-0123456"),
		ReturnTo:    "/settings",
	}
}

func rowCount(t *testing.T, pool *pgxpool.Pool, table, ref string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE server_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPendingAuthorizationIsSingleUse(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	clock := newClock()
	store := newTestStore(t, pool, clock)
	f := newFixture()
	ref := f.target("gateway").Ref

	state, err := store.CreatePendingAuthorization(ctx, f.pending(ref))
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
	want := f.pending(ref)
	if got.ServerRef != ref || got.ServerURL != testServerURL || got.Verifier.Reveal() != want.Verifier.Reveal() || got.Client.Secret.Reveal() != "client-secret-1" ||
		got.Client.TokenEndpointAuthMethod != "client_secret_basic" || got.RedirectURI != want.RedirectURI || got.ReturnTo != "/settings" {
		t.Fatalf("consumed = %+v", got)
	}
	if _, err := store.ConsumePendingAuthorization(ctx, state, f.ownerID); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("second use: err = %v", err)
	}

	// Another user's callback cannot use it, and the attempt burns it.
	state, err = store.CreatePendingAuthorization(ctx, f.pending(ref))
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
	state, err = store.CreatePendingAuthorization(ctx, f.pending(ref))
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
	stale, err := store.CreatePendingAuthorization(ctx, f.pending(ref))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(PendingTTL + time.Second)
	if _, err := store.CreatePendingAuthorization(ctx, f.pending(ref)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_state WHERE state_hash = $1`, stateHash(stale)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("an expired state was not cleared")
	}

	// Someone else's abandoned attempt on another entry goes too, in the
	// bounded batch every start clears.
	other := newFixture()
	abandoned, err := store.CreatePendingAuthorization(ctx, other.pending(WorkspaceRef(uuid.NewString())))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(PendingTTL + time.Second)
	if _, err := store.CreatePendingAuthorization(ctx, f.pending(ref)); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_state WHERE state_hash = $1`, stateHash(abandoned)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("another user's abandoned state was never cleared")
	}

	// A ref that is not one is refused before anything is stored.
	if _, err := store.CreatePendingAuthorization(ctx, f.pending("gateway")); err == nil {
		t.Fatal("a pending authorization with an invalid ref was stored")
	}
}

func TestGrantIsSealedAndReplacedOnReconnect(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	clock := newClock()
	store := newTestStore(t, pool, clock)
	f := newFixture()
	ref := f.target("gateway").Ref

	if _, err := store.GetConnectionStatus(ctx, f.workspaceID, ref, f.ownerID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("before connect: err = %v", err)
	}
	if err := store.SaveGrant(ctx, f.grant(ref, "access-token-one", "refresh-token-one", 3600)); err != nil {
		t.Fatal(err)
	}
	var access, refresh, clientSecret []byte
	if err := pool.QueryRow(ctx, `
		SELECT access_token_ciphertext, refresh_token_ciphertext, client_secret_ciphertext
		FROM mcp_oauth_connection WHERE server_ref = $1`, ref).Scan(&access, &refresh, &clientSecret); err != nil {
		t.Fatal(err)
	}
	for name, sealed := range map[string][]byte{"access": access, "refresh": refresh, "client secret": clientSecret} {
		if len(sealed) == 0 || bytes.Contains(sealed, []byte("token-one")) || bytes.Contains(sealed, []byte("client-secret-1")) {
			t.Errorf("%s token is missing or stored in plaintext", name)
		}
	}
	status, err := store.GetConnectionStatus(ctx, f.workspaceID, ref, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusConnected || status.ServerURL != testServerURL || status.ExpiresAt == nil ||
		!status.ExpiresAt.Equal(clock.Now().Add(time.Hour)) || status.GrantedScope != "mcp" {
		t.Fatalf("status = %+v", status)
	}
	// Read against an entry whose URL moved, it is a connection to reconnect.
	if moved := status.ForURL("https://elsewhere.example.com/mcp"); moved.Status != StatusNeedsReconnect || moved.LastError != ReasonURLChanged {
		t.Fatalf("status for another URL = %+v", moved)
	}
	if same := status.ForURL(testServerURL); same.Status != StatusConnected {
		t.Fatalf("status for its own URL = %+v", same)
	}

	// A token endpoint that gives no lifetime leaves expires_at empty.
	clock.Advance(time.Minute)
	if err := store.SaveGrant(ctx, f.grant(ref, "access-token-two", "", 0)); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetConnectionStatus(ctx, f.workspaceID, ref, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ExpiresAt != nil || !status.ConnectedAt.Equal(clock.Now()) {
		t.Fatalf("after reconnect: %+v", status)
	}
	other := f.target("other").Ref
	statuses, err := store.ListConnectionStatuses(ctx, f.workspaceID, f.ownerID, []string{ref, other})
	if err != nil || len(statuses) != 1 || statuses[ref].ServerRef != ref {
		t.Fatalf("list statuses = %+v, err = %v", statuses, err)
	}
	if statuses, err := store.ListConnectionStatuses(ctx, f.workspaceID, uuid.NewString(), []string{ref}); err != nil || len(statuses) != 0 {
		t.Fatalf("another user's statuses = %+v, err = %v", statuses, err)
	}

	// Disconnecting also cancels this user's connects still in flight.
	pending, err := store.CreatePendingAuthorization(ctx, f.pending(ref))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteConnection(ctx, f.workspaceID, ref, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumePendingAuthorization(ctx, pending, f.ownerID); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("a connect in flight survived the disconnect: %v", err)
	}
	if err := store.DeleteConnection(ctx, f.workspaceID, ref, f.ownerID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("second disconnect: err = %v", err)
	}
}

// One workspace entry has one connection per user; deleting the entry takes
// every user's.
func TestWorkspaceEntryConnectionsPerUser(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f := newFixture()
	second := f
	second.ownerID = uuid.NewString()
	ref := WorkspaceRef(uuid.NewString())
	for _, fx := range []fixture{f, second} {
		if err := store.SaveGrant(ctx, fx.grant(ref, "at-"+fx.ownerID, "rt", 3600)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreatePendingAuthorization(ctx, fx.pending(ref)); err != nil {
			t.Fatal(err)
		}
	}
	if n := rowCount(t, pool, "mcp_oauth_connection", ref); n != 2 {
		t.Fatalf("%d connections, want one per user", n)
	}
	// One user's disconnect leaves the other's.
	if removed, err := DeleteUserConnection(ctx, pool, f.workspaceID, ref, f.ownerID); err != nil || !removed {
		t.Fatalf("DeleteUserConnection = %v, %v", removed, err)
	}
	if _, err := store.GetConnectionStatus(ctx, f.workspaceID, ref, second.ownerID); err != nil {
		t.Fatalf("the other user's connection: %v", err)
	}
	if err := DeleteEntryData(ctx, pool, f.workspaceID, ref); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"mcp_oauth_connection", "mcp_oauth_state"} {
		if n := rowCount(t, pool, table, ref); n != 0 {
			t.Errorf("%s keeps %d rows of the deleted entry", table, n)
		}
	}
}

// Connections to an agent's own entries are forgotten when the entries are,
// and nothing else is.
func TestDeleteAgentEntryDataExcept(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f := newFixture()
	other := f
	other.agentID = uuid.NewString()
	kept, dropped := f.target("kept").Ref, f.target("dropped").Ref
	otherAgent := other.target("dropped").Ref
	workspace := WorkspaceRef(uuid.NewString())
	for _, ref := range []string{kept, dropped, otherAgent, workspace} {
		if err := store.SaveGrant(ctx, f.grant(ref, "at", "rt", 3600)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreatePendingAuthorization(ctx, f.pending(dropped)); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAgentEntryDataExcept(ctx, pool, f.workspaceID, f.agentID, []string{kept}); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]int{kept: 1, dropped: 0, otherAgent: 1, workspace: 1} {
		if n := rowCount(t, pool, "mcp_oauth_connection", ref); n != want {
			t.Errorf("%s: %d connections, want %d", ref, n, want)
		}
	}
	if n := rowCount(t, pool, "mcp_oauth_state", dropped); n != 0 {
		t.Error("a connect in flight to a removed entry survived")
	}
	// No entry left at all.
	if err := DeleteAgentEntryDataExcept(ctx, pool, f.workspaceID, f.agentID, nil); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, pool, "mcp_oauth_connection", kept); n != 0 {
		t.Error("the last entry's connection survived")
	}
}

func TestDeleteWorkspaceData(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := newTestStore(t, pool, newClock())
	f, other := newFixture(), newFixture()
	for _, fx := range []fixture{f, other} {
		ref := fx.target("gateway").Ref
		if err := store.SaveGrant(ctx, fx.grant(ref, "at", "rt", 3600)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreatePendingAuthorization(ctx, fx.pending(ref)); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteWorkspaceData(ctx, pool, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"mcp_oauth_connection", "mcp_oauth_state"} {
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
