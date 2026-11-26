// Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Token endpoint client authentication methods (RFC 7591 §2) an entry may
// ask for. The connection tables allow exactly these.
const (
	AuthMethodNone              = "none"
	AuthMethodClientSecretBasic = "client_secret_basic"
	AuthMethodClientSecretPost  = "client_secret_post"
)

// ClientConfig is how an entry's OAuth client is obtained. Without an ID the
// connect registers a client dynamically (RFC 7591), as before; with one, the
// entry names a client registered in advance with Issuer:
//
//	"auth": {"type": "oauth", "client_id": "...", "issuer": "https://...",
//	         "token_endpoint_auth_method": "client_secret_basic"}
//
// The client secret, if the client has one, is never part of the entry: it
// is stored sealed in mcp_oauth_client (SetClientSecret) and never returned.
type ClientConfig struct {
	ID string
	// Issuer is the authorization server the client is registered with.
	// The MCP authorization spec requires pre-registered credentials to be
	// used only with that server, so it is required with ID.
	Issuer string
	// TokenEndpointAuthMethod is the method the entry asks for; empty leaves
	// it to the connect (preregisteredAuthMethod).
	TokenEndpointAuthMethod string
}

// Preregistered reports whether the entry names a client registered in
// advance rather than registering one at connect time.
func (c ClientConfig) Preregistered() bool { return c.ID != "" }

const (
	maxClientIDBytes     = 2048
	maxClientSecretBytes = 4096
	maxIssuerBytes       = 2048
)

// Errors of the pre-registered client path. Their text is safe to show.
var (
	ErrClientSecretMissing = errors.New("the pre-registered client needs its client secret: set it on the MCP entry")
	// ErrClientSecretStale: the stored secret was set for another client id
	// or issuer than the entry names now. It is never sent; it has to be set
	// again for the new client.
	ErrClientSecretStale = errors.New("the client secret was set for another client ID or issuer: set it again on the MCP entry")
	ErrIssuerMismatch    = errors.New("the MCP server's authorization server is not the issuer the pre-registered client is registered with")
	// ErrNotPreregistered: a client secret was written for an entry that
	// names no pre-registered client.
	ErrNotPreregistered = errors.New("the MCP entry does not name a pre-registered client (auth.client_id)")
)

// ReasonClientChanged: the entry now names another client (client_id,
// issuer, token endpoint auth method or client secret) than the grant was
// made with. Only a new connect, with the new client, brings it back.
const ReasonClientChanged = "client_changed"

// isVSChars reports whether s is non-empty and only has the characters
// RFC 6749 Appendix A allows in a client_id or client_secret: %x20-7E.
func isVSChars(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// parseClientField reads one of the client keys of an entry's auth object
// into c. It reports false for a key that is not a client key.
func parseClientField(key string, value json.RawMessage, c *ClientConfig) (bool, error) {
	switch key {
	case "client_id":
		var id string
		if json.Unmarshal(value, &id) != nil {
			return true, fmt.Errorf("%w: auth.client_id must be a string", ErrInvalidInput)
		}
		id = strings.TrimSpace(id)
		if len(id) > maxClientIDBytes || !isVSChars(id) {
			return true, fmt.Errorf("%w: auth.client_id must be 1 to %d printable ASCII characters", ErrInvalidInput, maxClientIDBytes)
		}
		c.ID = id
	case "issuer":
		var issuer string
		if json.Unmarshal(value, &issuer) != nil {
			return true, fmt.Errorf("%w: auth.issuer must be a string", ErrInvalidInput)
		}
		issuer = strings.TrimSpace(issuer)
		if err := validateIssuer(issuer); err != nil {
			return true, err
		}
		c.Issuer = issuer
	case "token_endpoint_auth_method":
		var method string
		if json.Unmarshal(value, &method) != nil {
			return true, fmt.Errorf("%w: auth.token_endpoint_auth_method must be a string", ErrInvalidInput)
		}
		switch method {
		case AuthMethodNone, AuthMethodClientSecretBasic, AuthMethodClientSecretPost:
			c.TokenEndpointAuthMethod = method
		default:
			return true, fmt.Errorf("%w: auth.token_endpoint_auth_method must be %q, %q or %q", ErrInvalidInput,
				AuthMethodClientSecretBasic, AuthMethodClientSecretPost, AuthMethodNone)
		}
	case "client_secret":
		// The entry is readable configuration; a secret in it would be
		// copied, exported and shown.
		return true, fmt.Errorf("%w: auth.client_secret is never stored in the entry; set the entry's client secret on its own instead", ErrInvalidInput)
	default:
		return false, nil
	}
	return true, nil
}

// validate checks the client keys against each other.
func (c ClientConfig) validate() error {
	if c.ID == "" {
		if c.Issuer != "" || c.TokenEndpointAuthMethod != "" {
			return fmt.Errorf("%w: auth.issuer and auth.token_endpoint_auth_method need auth.client_id", ErrInvalidInput)
		}
		return nil
	}
	if c.Issuer == "" {
		return fmt.Errorf("%w: auth.issuer is required with auth.client_id: a pre-registered client belongs to one authorization server", ErrInvalidInput)
	}
	return nil
}

// validateIssuer checks the shape of an authorization server's issuer
// identifier (RFC 8414 §2): HTTPS, no query or fragment, no userinfo.
// Whether its host may be reached is decided each time it is contacted.
func validateIssuer(raw string) error {
	u, err := url.Parse(raw)
	if raw == "" || len(raw) > maxIssuerBytes || err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%w: auth.issuer must be an HTTPS URL without userinfo, query or fragment", ErrInvalidInput)
	}
	return nil
}

// preregisteredAuthMethod is the token endpoint auth method a pre-registered
// client uses. An entry that names one gets it, provided a method that sends
// a secret has one. Otherwise: no secret means a public client ("none"); with
// a secret, client_secret_basic when the authorization server lists it or
// lists nothing (RFC 8414 §2 defaults to it), else client_secret_post.
func preregisteredAuthMethod(explicit string, hasSecret bool, supported []string) (string, error) {
	if explicit != "" {
		if explicit != AuthMethodNone && !hasSecret {
			return "", ErrClientSecretMissing
		}
		return explicit, nil
	}
	switch {
	case !hasSecret:
		return AuthMethodNone, nil
	case len(supported) == 0 || slices.Contains(supported, AuthMethodClientSecretBasic):
		return AuthMethodClientSecretBasic, nil
	case slices.Contains(supported, AuthMethodClientSecretPost):
		return AuthMethodClientSecretPost, nil
	default:
		return "", fmt.Errorf("%w: the authorization server accepts neither %s nor %s", ErrUnsupportedClient,
			AuthMethodClientSecretBasic, AuthMethodClientSecretPost)
	}
}

// clientMatches reports whether a grant made with clientID and authMethod
// may serve the client the entry names now. A dynamically registered entry
// takes any: its client is whatever the last connect registered. A change
// from a pre-registered client back to dynamic registration, or of the
// issuer, is caught when the entry is saved (ClientChanged).
func (c ClientConfig) clientMatches(clientID, authMethod string) bool {
	if !c.Preregistered() {
		return true
	}
	if clientID != c.ID {
		return false
	}
	return c.TokenEndpointAuthMethod == "" || authMethod == c.TokenEndpointAuthMethod
}

// MatchesEntry reports whether a pending authorization may still complete
// for the entry as it is now: same URL, and the client the entry names.
func (p PendingAuthorization) MatchesEntry(entry Entry) bool {
	return p.ServerURL == entry.URL && entry.Client.clientMatches(p.Client.ID, p.Client.TokenEndpointAuthMethod)
}

// ClientIdentityChanged reports whether saving after over before changes the
// client id or issuer of an entry that names a pre-registered client both
// times: its client secret belongs to the old client and is dropped.
func ClientIdentityChanged(before, after json.RawMessage) bool {
	if !IsOAuthEntry(before) || !IsOAuthEntry(after) {
		return false
	}
	old, oldErr := ParseEntry(before)
	next, nextErr := ParseEntry(after)
	if oldErr != nil || nextErr != nil {
		return false
	}
	return old.Client.ID != next.Client.ID || old.Client.Issuer != next.Client.Issuer
}

// ClientChanged reports whether saving after over before changes the OAuth
// client of an entry that uses OAuth both times. Connections made with the
// old client then need a reconnect (MarkEntryNeedsReconnect).
func ClientChanged(before, after json.RawMessage) bool {
	if !IsOAuthEntry(before) || !IsOAuthEntry(after) {
		return false
	}
	old, oldErr := ParseEntry(before)
	next, nextErr := ParseEntry(after)
	if oldErr != nil || nextErr != nil {
		return false
	}
	return old.Client != next.Client
}

// ValidateClientSecret checks a client secret as it is written.
func ValidateClientSecret(secret string) error {
	if len(secret) > maxClientSecretBytes || !isVSChars(secret) {
		return fmt.Errorf("%w: client_secret must be 1 to %d printable ASCII characters", ErrInvalidInput, maxClientSecretBytes)
	}
	return nil
}

// SetClientSecret stores secret, sealed, as the client secret of the entry
// ref names, replacing any earlier one, bound to the pre-registered client
// the entry names now (client id and issuer): it is only ever used for that
// client. Every connection to the entry was made with the old client and
// needs a reconnect, and connects in flight are cancelled: they carry the old
// secret.
func (s *Store) SetClientSecret(ctx context.Context, workspaceID, ref, userID string, client ClientConfig, secret Secret) error {
	if _, ok := ParseRef(ref); !ok {
		return errors.New("client secret has an invalid server ref")
	}
	if !client.Preregistered() || client.Issuer == "" {
		return ErrNotPreregistered
	}
	if err := ValidateClientSecret(secret.Reveal()); err != nil {
		return err
	}
	sealed, err := s.seal(secret.Reveal())
	if err != nil {
		return err
	}
	now := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		INSERT INTO mcp_oauth_client (server_ref, workspace_id, client_id, issuer, client_secret_ciphertext, updated_by, updated_at)
		VALUES ($1, $2::uuid, $6, $7, $3, $4::uuid, $5)
		ON CONFLICT (server_ref) DO UPDATE SET
			client_id = EXCLUDED.client_id,
			issuer = EXCLUDED.issuer,
			client_secret_ciphertext = EXCLUDED.client_secret_ciphertext,
			updated_by = EXCLUDED.updated_by,
			updated_at = EXCLUDED.updated_at
		WHERE mcp_oauth_client.workspace_id = EXCLUDED.workspace_id`,
		ref, workspaceID, sealed, userID, now, client.ID, client.Issuer)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("client secret belongs to another workspace")
	}
	if err := MarkEntryNeedsReconnect(ctx, tx, workspaceID, ref, ReasonClientChanged, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteClientSecret removes the client secret of the entry ref names, if
// any, with the same effect on connections as SetClientSecret. It reports
// whether there was one.
func (s *Store) DeleteClientSecret(ctx context.Context, workspaceID, ref string) (bool, error) {
	now := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM mcp_oauth_client WHERE workspace_id = $1::uuid AND server_ref = $2`, workspaceID, ref)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if err := MarkEntryNeedsReconnect(ctx, tx, workspaceID, ref, ReasonClientChanged, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// clientSecret opens the client secret of the entry ref names for client;
// empty when none is set. A secret set for another client id or issuer is
// ErrClientSecretStale and is not opened.
func (s *Store) clientSecret(ctx context.Context, workspaceID, ref string, client ClientConfig) (Secret, error) {
	var (
		sealed        []byte
		clientID, iss string
	)
	err := s.db.QueryRow(ctx, `
		SELECT client_secret_ciphertext, client_id, issuer FROM mcp_oauth_client
		WHERE workspace_id = $1::uuid AND server_ref = $2`, workspaceID, ref).Scan(&sealed, &clientID, &iss)
	if errors.Is(err, pgx.ErrNoRows) {
		return Secret{}, nil
	}
	if err != nil {
		return Secret{}, err
	}
	if clientID != client.ID || iss != client.Issuer {
		return Secret{}, ErrClientSecretStale
	}
	plain, err := s.open(sealed)
	if err != nil {
		return Secret{}, fmt.Errorf("open client secret: %w", err)
	}
	return NewSecret(plain), nil
}

// ClientSecretBindings returns, for each of refs that has a client secret,
// the client (id and issuer) it was set for. It reads no secret and needs no
// key. A secret only counts as set for an entry whose client is that one
// (SecretSetFor).
func ClientSecretBindings(ctx context.Context, q Querier, workspaceID string, refs []string) (map[string]ClientConfig, error) {
	out := map[string]ClientConfig{}
	if len(refs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT server_ref, client_id, issuer FROM mcp_oauth_client
		WHERE workspace_id = $1::uuid AND server_ref = ANY($2::text[])`, workspaceID, refs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ref     string
			binding ClientConfig
		)
		if err := rows.Scan(&ref, &binding.ID, &binding.Issuer); err != nil {
			return nil, err
		}
		out[ref] = binding
	}
	return out, rows.Err()
}

// SecretSetFor reports whether a secret bound to binding (found reports
// there is one) is usable by the entry's client.
func SecretSetFor(binding ClientConfig, found bool, entry Entry) bool {
	return found && entry.Client.Preregistered() && binding.ID == entry.Client.ID && binding.Issuer == entry.Client.Issuer
}

// MarkEntryNeedsReconnect drops the tokens of every connection to an entry,
// recording reason, and cancels the connects to it in flight. It needs no
// key, so it can run inside another write's transaction.
func MarkEntryNeedsReconnect(ctx context.Context, q Querier, workspaceID, ref, reason string, now time.Time) error {
	if _, err := q.Exec(ctx, `
		DELETE FROM mcp_oauth_state WHERE workspace_id = $1::uuid AND server_ref = $2`, workspaceID, ref); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		UPDATE mcp_oauth_connection SET
			status = 'needs_reconnect',
			access_token_ciphertext = NULL,
			refresh_token_ciphertext = NULL,
			expires_at = NULL,
			last_error = $3,
			updated_at = $4
		WHERE workspace_id = $1::uuid AND server_ref = $2 AND status = 'connected'`,
		workspaceID, ref, reason, now)
	return err
}

// ForgetClientSecret deletes the client secret of an entry that no longer
// names a pre-registered client. It needs no key.
func ForgetClientSecret(ctx context.Context, q Querier, workspaceID, ref string) error {
	_, err := q.Exec(ctx, `DELETE FROM mcp_oauth_client WHERE workspace_id = $1::uuid AND server_ref = $2`, workspaceID, ref)
	return err
}
