// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// SecretKeyEnv is the base64 32-byte secretbox key that seals every token,
// client secret, PKCE verifier and extra header value. Without it the feature
// stays unavailable.
const SecretKeyEnv = "MULTICA_MCP_OAUTH_SECRET_KEY"

// PendingTTL is how long a started authorization may take to come back.
const PendingTTL = 10 * time.Minute

// Connection statuses.
const (
	StatusConnected      = "connected"
	StatusNeedsReconnect = "needs_reconnect"
)

// Reasons stored in last_error. They are fixed codes chosen here, never an
// error string from the network, so nothing a server sends back (or a token
// echoed in an error) can reach the database or the API.
const (
	ReasonRefreshRejected = "refresh_rejected"
	ReasonRefreshFailed   = "refresh_failed"
	ReasonNoRefreshToken  = "no_refresh_token"
	ReasonUndecryptable   = "undecryptable"
)

var (
	ErrNotFound     = errors.New("MCP OAuth server not found")
	ErrNameTaken    = errors.New("this agent already has an MCP server with that name")
	ErrNotConnected = errors.New("MCP OAuth server is not connected")
	ErrStateInvalid = errors.New("authorization state is invalid, used or expired")
	ErrNoKey        = errors.New(SecretKeyEnv + " is not configured")
)

// Querier is the slice of a pgx pool or transaction the store uses. All SQL
// here is hand-written against tables sqlc does not see (migration 9023).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Database is what the server hands this package: a pgx pool.
type Database interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store reads and writes the MCP OAuth tables. Secrets are sealed on the way
// in and opened only by TokenSource and the callback that needs them.
type Store struct {
	db  Database
	box *secretbox.Box
	now func() time.Time
}

func NewStore(db Database, box *secretbox.Box) (*Store, error) {
	if box == nil {
		return nil, ErrNoKey
	}
	return &Store{db: db, box: box, now: time.Now}, nil
}

// Server is an OAuth MCP server definition. Extra header values are not part
// of it: they are write-only.
type Server struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	AgentID         string    `json:"agent_id"`
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	Scope           string    `json:"scope"`
	HasExtraHeaders bool      `json:"has_extra_headers"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ServerInput creates a server. ExtraHeaders are sent with every MCP request
// next to the OAuth token (for example a gateway's own API key header).
type ServerInput struct {
	WorkspaceID  string
	AgentID      string
	CreatedBy    string
	Name         string
	URL          string
	Scope        string
	ExtraHeaders map[string]string
}

// ConnectionStatus is what may be shown about a connection: no token, no
// client secret.
type ConnectionStatus struct {
	ServerID     string     `json:"server_id"`
	UserID       string     `json:"user_id"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	GrantedScope string     `json:"granted_scope"`
	LastError    string     `json:"last_error,omitempty"`
	ConnectedAt  time.Time  `json:"connected_at"`
	RefreshedAt  *time.Time `json:"refreshed_at,omitempty"`
}

// Client is an OAuth client registration as the connect flow obtained it.
type Client struct {
	ID                      string
	Secret                  Secret
	TokenEndpointAuthMethod string
}

func (c Client) registration() remotemcp.OAuthClientRegistration {
	method := c.TokenEndpointAuthMethod
	if method == "" {
		method = "none"
	}
	return remotemcp.OAuthClientRegistration{ClientID: c.ID, ClientSecret: c.Secret.Reveal(), TokenEndpointAuthMethod: method}
}

// PendingAuthorization is one started authorization, kept until its callback.
type PendingAuthorization struct {
	WorkspaceID   string
	ServerID      string
	UserID        string
	Resource      string
	TokenEndpoint string
	Client        Client
	RedirectURI   string
	Verifier      Secret
	ReturnTo      string
	ExpiresAt     time.Time
}

// Grant is the result of a completed authorization.
type Grant struct {
	WorkspaceID   string
	ServerID      string
	UserID        string
	Resource      string
	TokenEndpoint string
	Client        Client
	AccessToken   Secret
	RefreshToken  Secret
	// ExpiresIn is the token endpoint's expires_in in seconds; 0 means it
	// did not say.
	ExpiresIn int64
	Scope     string
}

func (s *Store) seal(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	return s.box.Seal([]byte(value))
}

func (s *Store) open(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	plain, err := s.box.Open(sealed)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func normalizeAuthMethod(method string) (string, error) {
	switch method {
	case "", "none":
		return "none", nil
	case "client_secret_post", "client_secret_basic":
		return method, nil
	default:
		return "", fmt.Errorf("unsupported token endpoint auth method %q", method)
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

const serverColumns = `id::text, workspace_id::text, agent_id::text, name, url, scope,
	extra_headers_ciphertext IS NOT NULL, created_by::text, created_at, updated_at`

func scanServer(row pgx.Row) (Server, error) {
	var out Server
	err := row.Scan(&out.ID, &out.WorkspaceID, &out.AgentID, &out.Name, &out.URL, &out.Scope,
		&out.HasExtraHeaders, &out.CreatedBy, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	return out, err
}

// CreateServer stores a new server definition for an agent.
func (s *Store) CreateServer(ctx context.Context, in ServerInput) (Server, error) {
	if err := validateServerInput(in); err != nil {
		return Server{}, err
	}
	var headers []byte
	if len(in.ExtraHeaders) > 0 {
		raw, err := json.Marshal(in.ExtraHeaders)
		if err != nil {
			return Server{}, err
		}
		if headers, err = s.box.Seal(raw); err != nil {
			return Server{}, err
		}
	}
	now := s.now()
	out, err := scanServer(s.db.QueryRow(ctx, `
		INSERT INTO mcp_oauth_server (workspace_id, agent_id, name, url, scope, extra_headers_ciphertext, created_by, created_at, updated_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid, $8, $8)
		RETURNING `+serverColumns,
		in.WorkspaceID, in.AgentID, in.Name, in.URL, in.Scope, headers, in.CreatedBy, now))
	if isUniqueViolation(err) {
		return Server{}, ErrNameTaken
	}
	return out, err
}

// GetServer returns one server of the workspace.
func (s *Store) GetServer(ctx context.Context, workspaceID, serverID string) (Server, error) {
	return scanServer(s.db.QueryRow(ctx, `
		SELECT `+serverColumns+` FROM mcp_oauth_server
		WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspaceID, serverID))
}

// ListServersForAgent returns the agent's servers ordered by name.
func (s *Store) ListServersForAgent(ctx context.Context, workspaceID, agentID string) ([]Server, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+serverColumns+` FROM mcp_oauth_server
		WHERE workspace_id = $1::uuid AND agent_id = $2::uuid
		ORDER BY name`, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, rows.Err()
}

// DeleteServer removes a server with its connections and pending
// authorizations. There are no foreign keys, so this is the cascade.
func (s *Store) DeleteServer(ctx context.Context, workspaceID, serverID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM mcp_oauth_server WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspaceID, serverID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	for _, table := range []string{"mcp_oauth_connection", "mcp_oauth_state"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = $1::uuid AND server_id = $2::uuid`, workspaceID, serverID); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return tx.Commit(ctx)
}

// NewVerifier returns a PKCE code verifier: 32 random bytes, base64url, which
// is 43 characters from the unreserved set RFC 7636 requires.
func NewVerifier() (Secret, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Secret{}, err
	}
	return NewSecret(base64.RawURLEncoding.EncodeToString(raw)), nil
}

func stateHash(state string) []byte {
	sum := sha256.Sum256([]byte(state))
	return sum[:]
}

// CreatePendingAuthorization records a started authorization and returns the
// state to send to the authorization server. Only the state's hash is stored.
func (s *Store) CreatePendingAuthorization(ctx context.Context, p PendingAuthorization) (string, error) {
	if p.Verifier.Empty() || p.Client.ID == "" {
		return "", errors.New("pending authorization needs a verifier and a client id")
	}
	method, err := normalizeAuthMethod(p.Client.TokenEndpointAuthMethod)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	verifier, err := s.seal(p.Verifier.Reveal())
	if err != nil {
		return "", err
	}
	clientSecret, err := s.seal(p.Client.Secret.Reveal())
	if err != nil {
		return "", err
	}
	now := s.now()
	// Clear abandoned attempts as new ones start, so the table holds only
	// attempts in flight without a sweeper: this user's on this server, and a
	// bounded batch of anyone's. The table is that small, so the unindexed
	// scan for the batch stays cheap, and the bound keeps one start from
	// paying for a backlog.
	if _, err := s.db.Exec(ctx, `
		DELETE FROM mcp_oauth_state
		WHERE (server_id = $1::uuid AND user_id = $2::uuid AND expires_at < $3)
			OR state_hash IN (SELECT state_hash FROM mcp_oauth_state WHERE expires_at < $3 LIMIT 50)`,
		p.ServerID, p.UserID, now); err != nil {
		return "", err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO mcp_oauth_state (state_hash, workspace_id, server_id, user_id, resource, token_endpoint,
			client_id, client_secret_ciphertext, token_endpoint_auth_method, redirect_uri, verifier_ciphertext,
			return_to, expires_at, created_at)
		VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		stateHash(state), p.WorkspaceID, p.ServerID, p.UserID, p.Resource, p.TokenEndpoint,
		p.Client.ID, clientSecret, method, p.RedirectURI, verifier, p.ReturnTo, now.Add(PendingTTL), now)
	if err != nil {
		return "", err
	}
	return state, nil
}

// ConsumePendingAuthorization takes the authorization a callback's state
// belongs to. The row is deleted as it is read, so a state works once; a
// state that is unknown, expired, or was started by another user fails with
// ErrStateInvalid.
func (s *Store) ConsumePendingAuthorization(ctx context.Context, state, userID string) (PendingAuthorization, error) {
	if state == "" {
		return PendingAuthorization{}, ErrStateInvalid
	}
	var (
		p                      PendingAuthorization
		clientSecret, verifier []byte
	)
	err := s.db.QueryRow(ctx, `
		DELETE FROM mcp_oauth_state WHERE state_hash = $1
		RETURNING workspace_id::text, server_id::text, user_id::text, resource, token_endpoint,
			client_id, client_secret_ciphertext, token_endpoint_auth_method, redirect_uri,
			verifier_ciphertext, return_to, expires_at`, stateHash(state)).Scan(
		&p.WorkspaceID, &p.ServerID, &p.UserID, &p.Resource, &p.TokenEndpoint,
		&p.Client.ID, &clientSecret, &p.Client.TokenEndpointAuthMethod, &p.RedirectURI,
		&verifier, &p.ReturnTo, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PendingAuthorization{}, ErrStateInvalid
	}
	if err != nil {
		return PendingAuthorization{}, err
	}
	if p.UserID != userID || !s.now().Before(p.ExpiresAt) {
		return PendingAuthorization{}, ErrStateInvalid
	}
	secret, err := s.open(clientSecret)
	if err != nil {
		return PendingAuthorization{}, fmt.Errorf("open client secret: %w", err)
	}
	plainVerifier, err := s.open(verifier)
	if err != nil {
		return PendingAuthorization{}, fmt.Errorf("open verifier: %w", err)
	}
	p.Client.Secret = NewSecret(secret)
	p.Verifier = NewSecret(plainVerifier)
	return p, nil
}

// SaveGrant stores a completed authorization as the user's connection to the
// server, replacing any earlier one (a reconnect).
func (s *Store) SaveGrant(ctx context.Context, g Grant) error {
	if g.AccessToken.Empty() {
		return errors.New("grant has no access token")
	}
	method, err := normalizeAuthMethod(g.Client.TokenEndpointAuthMethod)
	if err != nil {
		return err
	}
	access, err := s.seal(g.AccessToken.Reveal())
	if err != nil {
		return err
	}
	refresh, err := s.seal(g.RefreshToken.Reveal())
	if err != nil {
		return err
	}
	clientSecret, err := s.seal(g.Client.Secret.Reveal())
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.db.Exec(ctx, `
		INSERT INTO mcp_oauth_connection (server_id, user_id, workspace_id, status, resource, token_endpoint,
			client_id, client_secret_ciphertext, token_endpoint_auth_method, access_token_ciphertext,
			refresh_token_ciphertext, expires_at, granted_scope, last_error, connected_at, refreshed_at, updated_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'connected', $4, $5, $6, $7, $8, $9, $10, $11, $12, '', $13, NULL, $13)
		ON CONFLICT (server_id, user_id) DO UPDATE SET
			workspace_id = EXCLUDED.workspace_id,
			status = 'connected',
			resource = EXCLUDED.resource,
			token_endpoint = EXCLUDED.token_endpoint,
			client_id = EXCLUDED.client_id,
			client_secret_ciphertext = EXCLUDED.client_secret_ciphertext,
			token_endpoint_auth_method = EXCLUDED.token_endpoint_auth_method,
			access_token_ciphertext = EXCLUDED.access_token_ciphertext,
			refresh_token_ciphertext = EXCLUDED.refresh_token_ciphertext,
			expires_at = EXCLUDED.expires_at,
			granted_scope = EXCLUDED.granted_scope,
			last_error = '',
			connected_at = EXCLUDED.connected_at,
			refreshed_at = NULL,
			updated_at = EXCLUDED.updated_at`,
		g.ServerID, g.UserID, g.WorkspaceID, g.Resource, g.TokenEndpoint,
		g.Client.ID, clientSecret, method, access, refresh, expiryArg(now, g.ExpiresIn), g.Scope, now)
	return err
}

func expiryArg(now time.Time, expiresIn int64) any {
	expiry := remotemcp.OAuthExpiry(now, expiresIn)
	if expiry.IsZero() {
		return nil
	}
	return expiry
}

const connectionStatusColumns = `server_id::text, user_id::text, status, expires_at, granted_scope,
	last_error, connected_at, refreshed_at`

func scanConnectionStatus(row pgx.Row) (ConnectionStatus, error) {
	var out ConnectionStatus
	err := row.Scan(&out.ServerID, &out.UserID, &out.Status, &out.ExpiresAt, &out.GrantedScope,
		&out.LastError, &out.ConnectedAt, &out.RefreshedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionStatus{}, ErrNotConnected
	}
	return out, err
}

// GetConnectionStatus returns the user's connection to a server, without
// secrets, or ErrNotConnected.
func (s *Store) GetConnectionStatus(ctx context.Context, workspaceID, serverID, userID string) (ConnectionStatus, error) {
	return scanConnectionStatus(s.db.QueryRow(ctx, `
		SELECT `+connectionStatusColumns+` FROM mcp_oauth_connection
		WHERE workspace_id = $1::uuid AND server_id = $2::uuid AND user_id = $3::uuid`,
		workspaceID, serverID, userID))
}

// ListConnectionStatusesForAgent returns every connection to the agent's
// servers, without secrets.
func (s *Store) ListConnectionStatusesForAgent(ctx context.Context, workspaceID, agentID string) ([]ConnectionStatus, error) {
	rows, err := s.db.Query(ctx, `
		SELECT c.server_id::text, c.user_id::text, c.status, c.expires_at, c.granted_scope,
			c.last_error, c.connected_at, c.refreshed_at
		FROM mcp_oauth_connection c
		JOIN mcp_oauth_server srv ON srv.id = c.server_id AND srv.workspace_id = c.workspace_id
		WHERE c.workspace_id = $1::uuid AND srv.agent_id = $2::uuid
		ORDER BY srv.name, c.user_id`, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConnectionStatus{}
	for rows.Next() {
		status, err := scanConnectionStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, rows.Err()
}

// DeleteConnection forgets the user's tokens for a server (disconnect), and
// cancels the user's connects to it still in flight: an authorization page
// left open in another tab must not reconnect what was just disconnected.
// It returns ErrNotConnected when there was no connection; pending
// authorizations are cancelled either way.
func (s *Store) DeleteConnection(ctx context.Context, workspaceID, serverID, userID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		DELETE FROM mcp_oauth_state
		WHERE workspace_id = $1::uuid AND server_id = $2::uuid AND user_id = $3::uuid`,
		workspaceID, serverID, userID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM mcp_oauth_connection
		WHERE workspace_id = $1::uuid AND server_id = $2::uuid AND user_id = $3::uuid`,
		workspaceID, serverID, userID)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotConnected
	}
	return nil
}

// DeleteWorkspaceData removes every MCP OAuth row of a workspace. It runs
// inside the workspace delete transaction; the tables have no foreign keys.
func DeleteWorkspaceData(ctx context.Context, q Querier, workspaceID any) error {
	for _, table := range []string{"mcp_oauth_state", "mcp_oauth_connection", "mcp_oauth_server"} {
		if _, err := q.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspaceID); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return nil
}
