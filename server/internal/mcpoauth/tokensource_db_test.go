// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: connections keyed by entry ref, bound to its URL (M-269).

package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// fakeAuthServer is a token endpoint that rotates refresh tokens: each
// refresh returns at-N / rt-N and only accepts the latest refresh token, as a
// server with reuse detection would.
type fakeAuthServer struct {
	mu        sync.Mutex
	current   string
	issued    int
	calls     atomic.Int32
	expiresIn int64
	rotate    bool
	// fail, when set, is returned instead of a token.
	fail error
	// gate, when set, blocks every refresh until it is closed.
	gate chan struct{}
	// during, when set, runs inside every refresh call, as time passing
	// while the token endpoint answers.
	during func()
	seen   []RefreshRequest
}

func (a *fakeAuthServer) refresh(ctx context.Context, req RefreshRequest) (remotemcp.OAuthTokenResponse, error) {
	a.calls.Add(1)
	if a.gate != nil {
		select {
		case <-a.gate:
		case <-ctx.Done():
			return remotemcp.OAuthTokenResponse{}, ctx.Err()
		}
	}
	if a.during != nil {
		a.during()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, req)
	if a.fail != nil {
		return remotemcp.OAuthTokenResponse{}, a.fail
	}
	if req.RefreshToken.Reveal() != a.current {
		return remotemcp.OAuthTokenResponse{}, fmt.Errorf("%w: reused refresh token", ErrGrantRejected)
	}
	a.issued++
	response := remotemcp.OAuthTokenResponse{
		AccessToken: fmt.Sprintf("at-%d", a.issued+1), TokenType: "Bearer", ExpiresIn: a.expiresIn,
	}
	if a.rotate {
		a.current = fmt.Sprintf("rt-%d", a.issued+1)
		response.RefreshToken = a.current
	}
	return response, nil
}

type tokenHarness struct {
	pool   *pgxpool.Pool
	clock  *testClock
	store  *Store
	auth   *fakeAuthServer
	source *TokenSource
	f      fixture
	target Target
}

// newTokenHarness connects the fixture owner with at-1 / rt-1, valid for
// lifetime from the clock's start.
func newTokenHarness(t *testing.T, lifetime time.Duration, refreshToken string) *tokenHarness {
	t.Helper()
	h := &tokenHarness{pool: testPool(t), clock: newClock(), f: newFixture()}
	h.store = newTestStore(t, h.pool, h.clock)
	h.auth = &fakeAuthServer{current: refreshToken, expiresIn: int64(lifetime / time.Second), rotate: true}
	h.source = NewTokenSource(h.store, h.auth.refresh, slog.New(slog.DiscardHandler))
	h.target = h.f.target("gateway")
	if err := h.store.SaveGrant(context.Background(), h.f.grant(h.target.Ref, "at-1", refreshToken, int64(lifetime/time.Second))); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *tokenHarness) credential(t *testing.T) (Credential, error) {
	t.Helper()
	return h.source.Credential(context.Background(), h.target, h.f.ownerID)
}

func (h *tokenHarness) mustToken(t *testing.T, want string) Credential {
	t.Helper()
	c, err := h.credential(t)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if got := c.AccessToken.Reveal(); got != want {
		t.Fatalf("access token = %q, want %q", got, want)
	}
	return c
}

func (h *tokenHarness) status(t *testing.T) ConnectionStatus {
	t.Helper()
	status, err := h.store.GetConnectionStatus(context.Background(), h.f.workspaceID, h.target.Ref, h.f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestFreshTokenIsNotRefreshed(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	c := h.mustToken(t, "at-1")
	if headers := c.Headers(); headers["Authorization"] != "Bearer at-1" || len(headers) != 1 {
		t.Fatalf("headers = %v", headers)
	}
	if !c.ExpiresAt.Equal(h.clock.Now().Add(time.Hour)) {
		t.Fatalf("expires at %v", c.ExpiresAt)
	}
	h.clock.Advance(time.Hour - DefaultMargin - time.Second)
	h.mustToken(t, "at-1")
	if n := h.auth.calls.Load(); n != 0 {
		t.Fatalf("%d refreshes for a fresh token", n)
	}
}

// Inside the margin the token is refreshed, the rotated refresh token is
// stored, and the next refresh presents it.
func TestRefreshRotatesAndPersistsTheRefreshToken(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.clock.Advance(time.Hour - DefaultMargin + time.Second)
	c := h.mustToken(t, "at-2")
	if !c.ExpiresAt.Equal(h.clock.Now().Add(time.Hour)) {
		t.Fatalf("refreshed token expires at %v", c.ExpiresAt)
	}
	// The stored token is the new one: no refresh until the next margin.
	h.mustToken(t, "at-2")
	h.clock.Advance(time.Hour - DefaultMargin + time.Second)
	h.mustToken(t, "at-3")

	if n := h.auth.calls.Load(); n != 2 {
		t.Fatalf("%d refreshes, want 2", n)
	}
	first, second := h.auth.seen[0], h.auth.seen[1]
	if first.RefreshToken.Reveal() != "rt-1" || second.RefreshToken.Reveal() != "rt-2" {
		t.Fatalf("refresh tokens presented: %q then %q", first.RefreshToken.Reveal(), second.RefreshToken.Reveal())
	}
	if first.TokenEndpoint != "https://auth.example.com/token" || first.Resource != testServerURL ||
		first.Client.ClientID != "client-1" || first.Client.ClientSecret != "client-secret-1" || first.Client.TokenEndpointAuthMethod != "client_secret_post" {
		t.Fatalf("refresh request = %+v", first)
	}
	status := h.status(t)
	if status.Status != StatusConnected || status.RefreshedAt == nil || status.LastError != "" {
		t.Fatalf("status = %+v", status)
	}
}

// A server that does not rotate omits refresh_token; the old one is kept.
func TestRefreshWithoutRotationKeepsTheRefreshToken(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.rotate = false
	for i := 2; i <= 3; i++ {
		h.clock.Advance(time.Hour)
		h.mustToken(t, fmt.Sprintf("at-%d", i))
	}
	for _, req := range h.auth.seen {
		if req.RefreshToken.Reveal() != "rt-1" {
			t.Fatalf("presented %q, want the unrotated rt-1", req.RefreshToken.Reveal())
		}
	}
}

// Many claims at once for an expiring token cause one refresh, and all of
// them get the new token. Without the row lock each would present rt-1; the
// fake server rejects reuse, which would mark the connection for reconnect.
func TestConcurrentClaimsRefreshOnce(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.gate = make(chan struct{})
	h.clock.Advance(time.Hour)

	const callers = 8
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := h.source.Credential(context.Background(), h.target, h.f.ownerID)
			errs[i] = err
			results[i] = c.AccessToken.Reveal()
		}()
	}
	// Let every caller reach the lock before the first refresh completes.
	time.Sleep(300 * time.Millisecond)
	close(h.auth.gate)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || results[i] != "at-2" {
			t.Fatalf("caller %d: token %q, err %v", i, results[i], errs[i])
		}
	}
	if n := h.auth.calls.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	if status := h.status(t); status.Status != StatusConnected {
		t.Fatalf("status = %+v", status)
	}
}

// A server issuing tokens shorter than the margin: a just-issued token is
// used, not refreshed again by every caller.
func TestShortLivedTokenIsUsedOnceIssued(t *testing.T) {
	h := newTokenHarness(t, time.Minute, "rt-1")
	h.mustToken(t, "at-1")
	h.clock.Advance(20 * time.Second)
	h.mustToken(t, "at-1")
	h.clock.Advance(15 * time.Second) // 25s left, inside half the lifetime
	h.mustToken(t, "at-2")
	h.mustToken(t, "at-2")
	if n := h.auth.calls.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

func TestRejectedRefreshNeedsReconnect(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.fail = fmt.Errorf("%w: HTTP 400", ErrGrantRejected)
	h.clock.Advance(time.Hour - time.Minute)
	if _, err := h.credential(t); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("err = %v, want ErrNeedsReconnect", err)
	}
	status := h.status(t)
	if status.Status != StatusNeedsReconnect || status.LastError != ReasonRefreshRejected || status.ExpiresAt != nil {
		t.Fatalf("status = %+v", status)
	}
	var tokens int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM mcp_oauth_connection
		WHERE server_ref = $1 AND (access_token_ciphertext IS NOT NULL OR refresh_token_ciphertext IS NOT NULL)`,
		h.target.Ref).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatal("a connection that needs reconnect still holds tokens")
	}
	// It stays that way without asking the server again.
	if _, err := h.credential(t); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("second call: err = %v", err)
	}
	if n := h.auth.calls.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	// Connecting again restores it.
	if err := h.store.SaveGrant(context.Background(), h.f.grant(h.target.Ref, "at-new", "rt-new", 3600)); err != nil {
		t.Fatal(err)
	}
	h.mustToken(t, "at-new")
	if status := h.status(t); status.Status != StatusConnected || status.LastError != "" {
		t.Fatalf("after reconnect: %+v", status)
	}
}

// A refresh that fails for another reason keeps the grant: the current token
// is used while it lasts, and once it has expired the server is unavailable,
// not disconnected.
func TestTransientRefreshFailureKeepsTheGrant(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.fail = errors.New("HTTP 503")
	h.clock.Advance(time.Hour - time.Minute)
	h.mustToken(t, "at-1")
	if status := h.status(t); status.Status != StatusConnected || status.LastError != ReasonRefreshFailed {
		t.Fatalf("status = %+v", status)
	}
	h.clock.Advance(2 * time.Minute)
	if _, err := h.credential(t); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired token: err = %v, want ErrUnavailable", err)
	}
	if status := h.status(t); status.Status != StatusConnected {
		t.Fatalf("status = %+v", status)
	}
	// The server recovers; once the retry window has passed the stored
	// refresh token still works.
	h.auth.fail = nil
	h.clock.Advance(DefaultRetryAfterFailure)
	h.mustToken(t, "at-2")
	if status := h.status(t); status.LastError != "" {
		t.Fatalf("status = %+v", status)
	}
}

// The old token expires while the failing refresh is in flight. It must be
// judged at the time the refresh failed: handing it out would mount a server
// whose every call is answered 401.
func TestTransientFailureAfterTheOldTokenExpired(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.fail = errors.New("HTTP 503")
	h.auth.during = func() { h.clock.Advance(5 * time.Second) }
	h.clock.Advance(time.Hour - 2*time.Second)
	if _, err := h.credential(t); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if status := h.status(t); status.Status != StatusConnected || status.LastError != ReasonRefreshFailed {
		t.Fatalf("status = %+v, want the grant kept", status)
	}
	// The grant still works once the server answers.
	h.auth.fail = nil
	h.auth.during = nil
	h.clock.Advance(DefaultRetryAfterFailure)
	h.mustToken(t, "at-2")
}

// After a transient failure, callers within the retry window use the current
// token without asking the token endpoint again.
func TestTransientFailureBacksOff(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.fail = errors.New("HTTP 503")
	h.clock.Advance(time.Hour - time.Minute)
	h.mustToken(t, "at-1")
	h.clock.Advance(10 * time.Second)
	h.mustToken(t, "at-1")
	if n := h.auth.calls.Load(); n != 1 {
		t.Fatalf("%d refreshes inside the retry window, want 1", n)
	}
	h.clock.Advance(DefaultRetryAfterFailure)
	h.auth.fail = nil
	h.mustToken(t, "at-2")
	if n := h.auth.calls.Load(); n != 2 {
		t.Fatalf("%d refreshes, want 2 once the window passed", n)
	}
}

func TestWithoutRefreshToken(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "")
	h.clock.Advance(time.Hour - time.Minute)
	// Inside the margin but not expired: what is left is handed out.
	h.mustToken(t, "at-1")
	h.clock.Advance(2 * time.Minute)
	if _, err := h.credential(t); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("err = %v, want ErrNeedsReconnect", err)
	}
	if status := h.status(t); status.LastError != ReasonNoRefreshToken {
		t.Fatalf("status = %+v", status)
	}
	if n := h.auth.calls.Load(); n != 0 {
		t.Fatalf("%d refreshes without a refresh token", n)
	}
}

// A caller that gives up does not abandon the refresh: the rotated refresh
// token is still saved, so the next caller is not forced to reconnect.
func TestCancelledCallerDoesNotLoseTheRotatedToken(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	h.auth.gate = make(chan struct{})
	h.clock.Advance(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := h.source.Credential(ctx, h.target, h.f.ownerID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	close(h.auth.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := h.status(t)
		if status.RefreshedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached refresh was never saved")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.mustToken(t, "at-2")
	if n := h.auth.calls.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

// Tokens sealed under another key cannot be used again.
func TestUndecryptableTokenNeedsReconnect(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	other, err := NewStore(h.pool, newBox(t))
	if err != nil {
		t.Fatal(err)
	}
	other.now = h.clock.Now
	source := NewTokenSource(other, h.auth.refresh, slog.New(slog.DiscardHandler))
	if _, err := source.Credential(context.Background(), h.target, h.f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("err = %v, want ErrNeedsReconnect", err)
	}
	if status := h.status(t); status.Status != StatusNeedsReconnect || status.LastError != ReasonUndecryptable {
		t.Fatalf("status = %+v", status)
	}
}

func TestCredentialForUnknownConnection(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	if _, err := h.source.Credential(context.Background(), h.target, h.f.agentID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	// The same user's grant for one entry is not another entry's.
	if _, err := h.source.Credential(context.Background(), h.f.target("other"), h.f.ownerID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("another entry: err = %v, want ErrNotConnected", err)
	}
}

// A token the MCP server refused is refreshed even though it has most of its
// lifetime left; a caller reporting a token someone already replaced gets the
// current one without another refresh; an unknown hash changes nothing.
func TestCredentialReplacingARejectedToken(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	ctx := context.Background()

	c, err := h.source.CredentialReplacing(ctx, h.target, h.f.ownerID, TokenHash("at-1"))
	if err != nil || c.AccessToken.Reveal() != "at-2" {
		t.Fatalf("after rejecting at-1: %q, %v; want at-2", c.AccessToken.Reveal(), err)
	}
	c, err = h.source.CredentialReplacing(ctx, h.target, h.f.ownerID, TokenHash("at-1"))
	if err != nil || c.AccessToken.Reveal() != "at-2" {
		t.Fatalf("stale rejection: %q, %v; want at-2", c.AccessToken.Reveal(), err)
	}
	c, err = h.source.CredentialReplacing(ctx, h.target, h.f.ownerID, TokenHash("some-other-token"))
	if err != nil || c.AccessToken.Reveal() != "at-2" {
		t.Fatalf("unknown hash: %q, %v; want at-2", c.AccessToken.Reveal(), err)
	}
	if n := h.auth.calls.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}

	// A rejected token is never handed back, not even while a transient
	// failure is being waited out.
	h.auth.fail = errors.New("HTTP 503")
	if _, err := h.source.CredentialReplacing(ctx, h.target, h.f.ownerID, TokenHash("at-2")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rejected token with the endpoint down: err = %v, want ErrUnavailable", err)
	}
	if _, err := h.source.CredentialReplacing(ctx, h.target, h.f.ownerID, TokenHash("at-2")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("inside the retry window: err = %v, want ErrUnavailable", err)
	}
	// Callers that were not refused still get the token while it lasts.
	h.mustToken(t, "at-2")
}

func TestTokenHash(t *testing.T) {
	if got := TokenHash("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("TokenHash = %s", got)
	}
}

// A grant is bound to the URL it was made for. Once the entry points
// elsewhere no token is handed out — not even a fresh one, and no refresh is
// tried — and the connection asks for a reconnect; pointing the entry back
// does not revive it.
func TestCredentialRefusesAnotherURL(t *testing.T) {
	h := newTokenHarness(t, time.Hour, "rt-1")
	moved := h.target
	moved.URL = "https://attacker.example.com/mcp"
	if _, err := h.source.Credential(context.Background(), moved, h.f.ownerID); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("err = %v, want ErrNeedsReconnect", err)
	}
	if n := h.auth.calls.Load(); n != 0 {
		t.Fatalf("%d refreshes for a moved entry", n)
	}
	status := h.status(t)
	if status.Status != StatusNeedsReconnect || status.LastError != ReasonURLChanged || status.ExpiresAt != nil {
		t.Fatalf("status = %+v", status)
	}
	if _, err := h.credential(t); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("back at the old URL: err = %v, want ErrNeedsReconnect", err)
	}

	// Inside the refresh too: a token due for refresh is not refreshed for
	// a moved entry.
	h = newTokenHarness(t, time.Hour, "rt-1")
	h.clock.Advance(time.Hour)
	moved = h.target
	moved.URL = "https://attacker.example.com/mcp"
	if _, err := h.source.CredentialReplacing(context.Background(), moved, h.f.ownerID, TokenHash("at-1")); !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("expired token, moved entry: err = %v, want ErrNeedsReconnect", err)
	}
	if n := h.auth.calls.Load(); n != 0 {
		t.Fatalf("%d refreshes for a moved entry", n)
	}
}
