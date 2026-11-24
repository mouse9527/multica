// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

var (
	// ErrNeedsReconnect means the stored grant cannot be used again: the
	// refresh token was rejected, there is none, or it cannot be decrypted.
	// Only the user connecting again fixes it.
	ErrNeedsReconnect = errors.New("MCP OAuth connection needs to be reconnected")
	// ErrUnavailable means no usable token right now, for a reason that may
	// pass (the authorization server did not answer). The grant is kept.
	ErrUnavailable = errors.New("MCP OAuth token is temporarily unavailable")
	// ErrServerUnreadable means the server's sealed extra headers cannot be
	// opened (the key changed). Reconnecting does not help; the server's
	// headers have to be saved again.
	ErrServerUnreadable = errors.New("MCP OAuth server extra headers cannot be decrypted")
	// ErrGrantRejected is what a Refresher returns, wrapped, when the
	// authorization server refused the refresh token itself (HTTP 400 or 401
	// from the token endpoint: invalid_grant, invalid_client). Any other
	// refresh error is treated as transient.
	ErrGrantRejected = errors.New("authorization server rejected the refresh token")
)

// RefreshRequest is everything a refresh needs. RefreshToken is the current
// one; when the server rotates it, the response carries the next.
type RefreshRequest struct {
	TokenEndpoint string
	Resource      string
	RefreshToken  Secret
	Client        remotemcp.OAuthClientRegistration
}

// Refresher performs the refresh_token grant against the token endpoint.
type Refresher func(ctx context.Context, req RefreshRequest) (remotemcp.OAuthTokenResponse, error)

// Credential is what a claim needs to reach the server for one task.
type Credential struct {
	AccessToken Secret
	// ExpiresAt is zero when the authorization server gave no lifetime.
	ExpiresAt    time.Time
	ExtraHeaders map[string]Secret
}

// Headers returns the HTTP headers to send: the server's extra headers and
// the bearer token. This is the only place the token is put into a header.
func (c Credential) Headers() map[string]string {
	out := make(map[string]string, len(c.ExtraHeaders)+1)
	for name, value := range c.ExtraHeaders {
		out[name] = value.Reveal()
	}
	out["Authorization"] = "Bearer " + c.AccessToken.Reveal()
	return out
}

// Default TokenSource settings.
const (
	DefaultMargin         = 5 * time.Minute
	DefaultRefreshTimeout = 10 * time.Second
	DefaultLockTimeout    = 15 * time.Second
	// DefaultRetryAfterFailure is how long a transient refresh failure is
	// remembered: within it, callers use the current token while it lasts
	// (or get ErrUnavailable) instead of each taking the lock and waiting on
	// a token endpoint that has just failed.
	DefaultRetryAfterFailure = 30 * time.Second
)

// commitReserve is the time kept, after the refresh call's own timeout, for
// the UPDATE and COMMIT that save its result. Without it a refresh that
// succeeded at the last moment could miss the deadline on the write, and the
// rotated refresh token — already accepted by the server — would be lost.
const commitReserve = 5 * time.Second

// TokenSource hands out access tokens, refreshing them on demand.
type TokenSource struct {
	store   *Store
	refresh Refresher
	logger  *slog.Logger
	// Margin: a token that expires within it is refreshed before it is
	// handed out.
	Margin time.Duration
	// RefreshTimeout bounds one call to the token endpoint.
	RefreshTimeout time.Duration
	// LockTimeout bounds the wait for another caller's refresh of the same
	// connection.
	LockTimeout time.Duration
	// RetryAfterFailure: see DefaultRetryAfterFailure.
	RetryAfterFailure time.Duration
}

func NewTokenSource(store *Store, refresh Refresher, logger *slog.Logger) *TokenSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &TokenSource{
		store: store, refresh: refresh, logger: logger,
		Margin: DefaultMargin, RefreshTimeout: DefaultRefreshTimeout, LockTimeout: DefaultLockTimeout,
		RetryAfterFailure: DefaultRetryAfterFailure,
	}
}

type connectionRow struct {
	status        string
	resource      string
	tokenEndpoint string
	clientID      string
	clientSecret  []byte
	authMethod    string
	access        []byte
	refresh       []byte
	expiresAt     *time.Time
	issuedAt      time.Time
	lastError     string
	updatedAt     time.Time
	extraHeaders  []byte
}

func loadConnection(ctx context.Context, q Querier, serverID, userID string, forUpdate bool) (connectionRow, error) {
	// The join reads the extra headers in the same statement; FOR UPDATE OF
	// c locks only the connection row.
	sql := `
		SELECT c.status, c.resource, c.token_endpoint, c.client_id, c.client_secret_ciphertext,
			c.token_endpoint_auth_method, c.access_token_ciphertext, c.refresh_token_ciphertext,
			c.expires_at, COALESCE(c.refreshed_at, c.connected_at), c.last_error, c.updated_at,
			srv.extra_headers_ciphertext
		FROM mcp_oauth_connection c
		JOIN mcp_oauth_server srv ON srv.id = c.server_id AND srv.workspace_id = c.workspace_id
		WHERE c.server_id = $1::uuid AND c.user_id = $2::uuid`
	if forUpdate {
		sql += ` FOR UPDATE OF c`
	}
	var row connectionRow
	err := q.QueryRow(ctx, sql, serverID, userID).Scan(&row.status, &row.resource, &row.tokenEndpoint,
		&row.clientID, &row.clientSecret, &row.authMethod, &row.access, &row.refresh, &row.expiresAt, &row.issuedAt, &row.lastError, &row.updatedAt, &row.extraHeaders)
	if errors.Is(err, pgx.ErrNoRows) {
		return connectionRow{}, ErrNotConnected
	}
	return row, err
}

// usable reports whether the stored access token may be handed out at now
// without refreshing: it exists and does not expire within the margin. A
// token without a stated lifetime is used until the server rejects it.
//
// The margin is capped at half the token's lifetime. Otherwise a server that
// issues tokens shorter than the margin would make every token look stale the
// moment it was issued, and each caller queued on the lock would refresh
// again instead of using the token the caller before it just got.
func (row connectionRow) usable(now time.Time, margin time.Duration) bool {
	if row.status != StatusConnected || len(row.access) == 0 {
		return false
	}
	if row.expiresAt == nil {
		return true
	}
	if lifetime := row.expiresAt.Sub(row.issuedAt); lifetime > 0 && lifetime/2 < margin {
		margin = lifetime / 2
	}
	return row.expiresAt.Sub(now) > margin
}

// stillValid reports whether the stored access token has not expired yet,
// even if it is inside the margin.
func (row connectionRow) stillValid(now time.Time) bool {
	if row.status != StatusConnected || len(row.access) == 0 {
		return false
	}
	return row.expiresAt == nil || row.expiresAt.After(now)
}

// recentlyFailed reports whether a transient refresh failure was recorded
// less than window ago.
func (row connectionRow) recentlyFailed(now time.Time, window time.Duration) bool {
	return row.lastError == ReasonRefreshFailed && now.Sub(row.updatedAt) < window
}

// Credential returns a credential for userID's connection to serverID.
//
// Errors: ErrNotConnected (no connection), ErrNeedsReconnect (the grant is
// gone; the row is marked so the settings page can say so), ErrUnavailable
// (try again later; the grant is kept). The caller decides what a task does
// without the server — the claim path mounts nothing and tells the task why.
func (s *TokenSource) Credential(ctx context.Context, serverID, userID string) (Credential, error) {
	return s.credentialReplacing(ctx, serverID, userID, "")
}

// CredentialReplacing is Credential for a caller whose last token was just
// refused by the MCP server (a 401). rejectedHash is TokenHash of that token.
// While it is still the stored token, it counts as expired: it is refreshed
// now rather than handed back, whatever its stated lifetime says. If someone
// already replaced it, the current token is returned without another refresh.
func (s *TokenSource) CredentialReplacing(ctx context.Context, serverID, userID, rejectedHash string) (Credential, error) {
	return s.credentialReplacing(ctx, serverID, userID, rejectedHash)
}

// TokenHash identifies an access token without carrying it: hex SHA-256.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// expireIfRejected treats the row's access token as already expired when it
// is the one the caller reported rejected, so every later check (usable,
// still valid, retry window) refuses to hand it out again.
func (s *TokenSource) expireIfRejected(row *connectionRow, rejectedHash string, now time.Time) {
	if rejectedHash == "" || len(row.access) == 0 {
		return
	}
	access, err := s.store.open(row.access)
	if err != nil || access == "" {
		return
	}
	if subtle.ConstantTimeCompare([]byte(TokenHash(access)), []byte(rejectedHash)) == 1 {
		expired := now
		row.expiresAt = &expired
	}
}

func (s *TokenSource) credentialReplacing(ctx context.Context, serverID, userID, rejectedHash string) (Credential, error) {
	row, err := loadConnection(ctx, s.store.db, serverID, userID, false)
	if err != nil {
		return Credential{}, err
	}
	now := s.store.now()
	if row.status == StatusNeedsReconnect {
		return Credential{}, ErrNeedsReconnect
	}
	s.expireIfRejected(&row, rejectedHash, now)
	if row.usable(now, s.Margin) {
		return s.credential(ctx, serverID, userID, row)
	}
	if row.recentlyFailed(now, s.RetryAfterFailure) {
		return s.afterRecentFailure(ctx, serverID, userID, row, now)
	}
	// The refresh runs detached (see refreshLocked); the caller waits for it
	// only as long as its own context allows.
	done := make(chan refreshResult, 1)
	go func() {
		credential, err := s.refreshLocked(ctx, serverID, userID, rejectedHash)
		done <- refreshResult{credential, err}
	}()
	select {
	case result := <-done:
		return result.credential, result.err
	case <-ctx.Done():
		return Credential{}, ErrUnavailable
	}
}

type refreshResult struct {
	credential Credential
	err        error
}

// refreshLocked refreshes under the connection's row lock. Whoever takes the
// lock first refreshes; everyone queued behind it re-reads the row once the
// lock is released and finds a fresh token, so one rotation of the refresh
// token happens per expiry however many claims arrive at once.
//
// The refresh and its write are detached from the caller's cancellation: once
// the token endpoint has rotated the refresh token, abandoning the write would
// lose the only copy of the new one and force a reconnect. RefreshTimeout
// and LockTimeout still bound it.
func (s *TokenSource) refreshLocked(ctx context.Context, serverID, userID, rejectedHash string) (Credential, error) {
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.LockTimeout+s.RefreshTimeout+commitReserve)
	defer cancel()
	tx, err := s.store.db.Begin(work)
	if err != nil {
		return Credential{}, err
	}
	defer func() { _ = tx.Rollback(work) }()
	if _, err := tx.Exec(work, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, s.LockTimeout.Milliseconds())); err != nil {
		return Credential{}, err
	}
	row, err := loadConnection(work, tx, serverID, userID, true)
	if err != nil {
		return Credential{}, s.unavailableOnLockTimeout(err)
	}
	now := s.store.now()
	if row.status == StatusNeedsReconnect {
		return Credential{}, ErrNeedsReconnect
	}
	s.expireIfRejected(&row, rejectedHash, now)
	if row.usable(now, s.Margin) {
		// Someone else refreshed while this caller waited for the lock.
		if err := tx.Commit(work); err != nil {
			return Credential{}, err
		}
		return s.credential(ctx, serverID, userID, row)
	}
	if row.recentlyFailed(now, s.RetryAfterFailure) {
		// The caller ahead of this one just failed; do not try again yet.
		if err := tx.Commit(work); err != nil {
			return Credential{}, err
		}
		return s.afterRecentFailure(ctx, serverID, userID, row, now)
	}

	refreshToken, err := s.store.open(row.refresh)
	if err != nil {
		return Credential{}, s.markNeedsReconnect(work, tx, serverID, userID, ReasonUndecryptable)
	}
	if refreshToken == "" {
		if row.stillValid(now) {
			// Nothing to refresh with; hand out what is left of this token.
			if err := tx.Commit(work); err != nil {
				return Credential{}, err
			}
			return s.credential(ctx, serverID, userID, row)
		}
		return Credential{}, s.markNeedsReconnect(work, tx, serverID, userID, ReasonNoRefreshToken)
	}
	clientSecret, err := s.store.open(row.clientSecret)
	if err != nil {
		return Credential{}, s.markNeedsReconnect(work, tx, serverID, userID, ReasonUndecryptable)
	}

	call, cancelCall := context.WithTimeout(work, s.RefreshTimeout)
	response, refreshErr := s.refresh(call, RefreshRequest{
		TokenEndpoint: row.tokenEndpoint,
		Resource:      row.resource,
		RefreshToken:  NewSecret(refreshToken),
		Client:        Client{ID: row.clientID, Secret: NewSecret(clientSecret), TokenEndpointAuthMethod: row.authMethod}.registration(),
	})
	cancelCall()
	if refreshErr != nil {
		if errors.Is(refreshErr, ErrGrantRejected) {
			return Credential{}, s.markNeedsReconnect(work, tx, serverID, userID, ReasonRefreshRejected)
		}
		// Transient. The error text is not logged: it comes from the network.
		s.logger.Warn("mcp oauth: refresh failed; keeping the grant", "server_id", serverID, "user_id", userID)
		// The call may have taken until RefreshTimeout, so whether the old
		// token is still usable is judged at the time it failed, not at the
		// time it started.
		failedAt := s.store.now()
		if _, err := tx.Exec(work, `
			UPDATE mcp_oauth_connection SET last_error = $3, updated_at = $4
			WHERE server_id = $1::uuid AND user_id = $2::uuid`, serverID, userID, ReasonRefreshFailed, failedAt); err != nil {
			return Credential{}, err
		}
		if err := tx.Commit(work); err != nil {
			return Credential{}, err
		}
		if row.stillValid(failedAt) {
			return s.credential(ctx, serverID, userID, row)
		}
		return Credential{}, ErrUnavailable
	}
	if response.AccessToken == "" {
		return Credential{}, ErrUnavailable
	}

	// A server that does not rotate omits refresh_token: keep the old one.
	nextRefresh := response.RefreshToken
	if nextRefresh == "" {
		nextRefresh = refreshToken
	}
	access, err := s.store.seal(response.AccessToken)
	if err != nil {
		return Credential{}, err
	}
	refreshSealed, err := s.store.seal(nextRefresh)
	if err != nil {
		return Credential{}, err
	}
	// The lifetime is counted from before the request was sent, so the
	// stored expiry is never later than the server's.
	expiry := expiryArg(now, response.ExpiresIn)
	if _, err := tx.Exec(work, `
		UPDATE mcp_oauth_connection SET
			access_token_ciphertext = $3,
			refresh_token_ciphertext = $4,
			expires_at = $5,
			granted_scope = CASE WHEN $6 = '' THEN granted_scope ELSE $6 END,
			last_error = '',
			refreshed_at = $7,
			updated_at = $7
		WHERE server_id = $1::uuid AND user_id = $2::uuid`,
		serverID, userID, access, refreshSealed, expiry, response.Scope, now); err != nil {
		return Credential{}, err
	}
	if err := tx.Commit(work); err != nil {
		return Credential{}, err
	}
	row.access = access
	row.issuedAt = now
	row.expiresAt = nil
	if t, ok := expiry.(time.Time); ok {
		row.expiresAt = &t
	}
	return s.credential(ctx, serverID, userID, row)
}

// afterRecentFailure answers without a refresh while a transient failure is
// recent: the current token while it is still valid, ErrUnavailable after.
func (s *TokenSource) afterRecentFailure(ctx context.Context, serverID, userID string, row connectionRow, now time.Time) (Credential, error) {
	if row.stillValid(now) {
		return s.credential(ctx, serverID, userID, row)
	}
	return Credential{}, ErrUnavailable
}

// markNeedsReconnect drops the tokens and records why, then commits. It
// returns ErrNeedsReconnect so callers can return it directly.
func (s *TokenSource) markNeedsReconnect(ctx context.Context, tx pgx.Tx, serverID, userID, reason string) error {
	s.logger.Warn("mcp oauth: connection needs reconnect", "server_id", serverID, "user_id", userID, "reason", reason)
	if _, err := tx.Exec(ctx, `
		UPDATE mcp_oauth_connection SET
			status = 'needs_reconnect',
			access_token_ciphertext = NULL,
			refresh_token_ciphertext = NULL,
			expires_at = NULL,
			last_error = $3,
			updated_at = $4
		WHERE server_id = $1::uuid AND user_id = $2::uuid`, serverID, userID, reason, s.store.now()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ErrNeedsReconnect
}

func (s *TokenSource) unavailableOnLockTimeout(err error) error {
	if isLockTimeout(err) {
		return ErrUnavailable
	}
	return err
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	// 55P03 lock_not_available: another refresh held the row past LockTimeout.
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

func (s *TokenSource) credential(ctx context.Context, serverID, userID string, row connectionRow) (Credential, error) {
	access, err := s.store.open(row.access)
	if err != nil || access == "" {
		return Credential{}, s.markUndecryptable(ctx, serverID, userID)
	}
	out := Credential{AccessToken: NewSecret(access)}
	if row.expiresAt != nil {
		out.ExpiresAt = *row.expiresAt
	}
	if len(row.extraHeaders) > 0 {
		raw, err := s.store.box.Open(row.extraHeaders)
		if err != nil {
			return Credential{}, ErrServerUnreadable
		}
		var headers map[string]string
		if err := json.Unmarshal(raw, &headers); err != nil {
			return Credential{}, ErrServerUnreadable
		}
		out.ExtraHeaders = make(map[string]Secret, len(headers))
		for name, value := range headers {
			out.ExtraHeaders[name] = NewSecret(value)
		}
	}
	return out, nil
}

// markUndecryptable handles a token sealed under another key: it can never
// be used again, so the connection asks for a reconnect.
func (s *TokenSource) markUndecryptable(ctx context.Context, serverID, userID string) error {
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.LockTimeout)
	defer cancel()
	tx, err := s.store.db.Begin(work)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(work) }()
	return s.markNeedsReconnect(work, tx, serverID, userID, ReasonUndecryptable)
}
