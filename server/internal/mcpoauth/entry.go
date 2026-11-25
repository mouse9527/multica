// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package mcpoauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// An MCP server entry — a value under mcpServers in an agent's mcp_config, or
// a workspace MCP library entry — uses OAuth when it declares
//
//	"auth": {"type": "oauth", "scope": "optional scopes"}
//
// next to its "url". The entry itself holds no credential: the tokens are in
// mcp_oauth_connection, keyed by the entry's ref and the connecting user.
const AuthTypeOAuth = "oauth"

// A ref names the entry a connection belongs to:
//
//	ws:<workspace_mcp_server.id>   a workspace library entry
//	agent:<agent id>:<entry name>  an entry of one agent's own mcp_config
//
// A workspace entry is connected once per user and shared by every agent of
// that user it is bound to; an agent's own entry is connected per agent.
const (
	workspaceRefPrefix = "ws:"
	agentRefPrefix     = "agent:"
)

// entryNamePattern is the name an OAuth entry may have: it becomes part of an
// agent ref and of the daemon's local proxy route.
var entryNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func WorkspaceRef(serverID string) string { return workspaceRefPrefix + serverID }

func AgentRef(agentID, name string) string { return AgentRefPrefix(agentID) + name }

// AgentRefPrefix is what every ref of the agent's own entries starts with.
func AgentRefPrefix(agentID string) string { return agentRefPrefix + agentID + ":" }

// Ref is a parsed ref: exactly one of WorkspaceServerID and AgentID is set.
type Ref struct {
	WorkspaceServerID string
	AgentID           string
	Name              string
}

// ParseRef reads a ref. Ids must be canonical UUID strings, so a ref has one
// spelling and compares as a string.
func ParseRef(ref string) (Ref, bool) {
	if id, ok := strings.CutPrefix(ref, workspaceRefPrefix); ok {
		if !isCanonicalUUID(id) {
			return Ref{}, false
		}
		return Ref{WorkspaceServerID: id}, true
	}
	rest, ok := strings.CutPrefix(ref, agentRefPrefix)
	if !ok {
		return Ref{}, false
	}
	agentID, name, ok := strings.Cut(rest, ":")
	if !ok || !isCanonicalUUID(agentID) || !entryNamePattern.MatchString(name) {
		return Ref{}, false
	}
	return Ref{AgentID: agentID, Name: name}, true
}

// A proxy handle names an entry together with the URL a claim gave a
// daemon's proxy for it: ref + "~" + URLDigest(url). The daemon passes it back
// verbatim when it fetches a token, so the server can refuse a token for any
// URL but the one that proxy sends to — the entry may be edited, and
// reconnected for its new URL, while the task still runs. "~" is unreserved,
// so the handle survives path escaping unchanged.
const proxyHandleSeparator = "~"

// URLDigest identifies an entry URL without spelling it out: hex SHA-256.
func URLDigest(entryURL string) string {
	sum := sha256.Sum256([]byte(entryURL))
	return hex.EncodeToString(sum[:])
}

// ProxyHandle is the handle for ref proxied to entryURL.
func ProxyHandle(ref, entryURL string) string {
	return ref + proxyHandleSeparator + URLDigest(entryURL)
}

// ParseProxyHandle splits a handle into its ref and URL digest.
func ParseProxyHandle(handle string) (ref, digest string, ok bool) {
	ref, digest, found := strings.Cut(handle, proxyHandleSeparator)
	if !found || len(digest) != sha256.Size*2 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", "", false
	}
	if _, valid := ParseRef(ref); !valid {
		return "", "", false
	}
	return ref, digest, true
}

func isCanonicalUUID(s string) bool {
	parsed, err := uuid.Parse(s)
	return err == nil && parsed.String() == s
}

// Entry is an OAuth MCP entry as a claim or a connect uses it.
type Entry struct {
	URL   string
	Scope string
	// Disabled is an entry switched off with "enabled": false or
	// "disabled": true; it is not mounted, so it needs no token.
	Disabled bool
}

// IsOAuthEntry reports whether raw asks for OAuth: an "auth" object whose
// type is "oauth" in any letter case. It is deliberately looser than
// ParseEntry, so an entry that only nearly follows the format is still
// treated as OAuth — withheld with a notice — and never mounted as a plain
// unauthenticated server.
func IsOAuthEntry(raw json.RawMessage) bool {
	var entry struct {
		Auth json.RawMessage `json:"auth"`
	}
	if json.Unmarshal(raw, &entry) != nil || !isJSONObject(entry.Auth) {
		return false
	}
	var auth struct {
		Type any `json:"type"`
	}
	if json.Unmarshal(entry.Auth, &auth) != nil {
		return false
	}
	kind, _ := auth.Type.(string)
	return strings.EqualFold(strings.TrimSpace(kind), AuthTypeOAuth)
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// ParseEntry reads an OAuth entry strictly. Every failure wraps
// ErrInvalidInput and its text names the field, never a value: entries can
// carry credentials.
func ParseEntry(raw json.RawMessage) (Entry, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Entry{}, fmt.Errorf("%w: the entry must be a JSON object", ErrInvalidInput)
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(fields["auth"], &auth); err != nil || auth == nil {
		return Entry{}, fmt.Errorf("%w: auth must be an object", ErrInvalidInput)
	}
	var out Entry
	for key, value := range auth {
		switch key {
		case "type":
			var kind string
			if json.Unmarshal(value, &kind) != nil || kind != AuthTypeOAuth {
				return Entry{}, fmt.Errorf("%w: auth.type must be %q", ErrInvalidInput, AuthTypeOAuth)
			}
		case "scope":
			if json.Unmarshal(value, &out.Scope) != nil {
				return Entry{}, fmt.Errorf("%w: auth.scope must be a string", ErrInvalidInput)
			}
			out.Scope = strings.TrimSpace(out.Scope)
			if err := validateScope(out.Scope); err != nil {
				return Entry{}, err
			}
		default:
			return Entry{}, fmt.Errorf("%w: auth.%s is not supported", ErrInvalidInput, key)
		}
	}
	if _, ok := fields["command"]; ok {
		return Entry{}, fmt.Errorf("%w: OAuth needs a remote (HTTP) MCP server, not a command", ErrInvalidInput)
	}
	if rawType, ok := fields["type"]; ok {
		var kind string
		if json.Unmarshal(rawType, &kind) != nil {
			return Entry{}, fmt.Errorf("%w: type must be a string", ErrInvalidInput)
		}
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "http", "streamable-http":
		default:
			return Entry{}, fmt.Errorf("%w: OAuth needs type \"http\"", ErrInvalidInput)
		}
	}
	// The token is the entry's only credential: the claim builds the
	// request headers from it alone.
	if rawHeaders, ok := fields["headers"]; ok {
		var headers map[string]json.RawMessage
		if json.Unmarshal(rawHeaders, &headers) != nil || len(headers) > 0 {
			return Entry{}, fmt.Errorf("%w: an OAuth entry takes no headers", ErrInvalidInput)
		}
	}
	var rawURL string
	if err := json.Unmarshal(fields["url"], &rawURL); err != nil {
		return Entry{}, fmt.Errorf("%w: url is required", ErrInvalidInput)
	}
	u, err := ValidateServerURL(rawURL)
	if err != nil {
		return Entry{}, err
	}
	out.URL = u.String()
	var enabled, disabled bool
	if raw, ok := fields["enabled"]; ok && json.Unmarshal(raw, &enabled) == nil && !enabled {
		out.Disabled = true
	}
	if raw, ok := fields["disabled"]; ok && json.Unmarshal(raw, &disabled) == nil && disabled {
		out.Disabled = true
	}
	return out, nil
}

// ValidateEntryForSave checks one entry as it is saved. An entry without an
// "auth" field is not this package's and passes untouched; one with it must
// be a well-formed OAuth entry whose URL passes the address rules that can be
// checked without a DNS lookup (EntryURLAllowed).
func ValidateEntryForSave(ctx context.Context, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil // not an object: the caller's own validation owns that
	}
	if _, ok := fields["auth"]; !ok {
		return nil
	}
	entry, err := ParseEntry(raw)
	if err != nil {
		return err
	}
	return EntryURLAllowed(ctx, entry.URL)
}

// ValidateConfigForSave runs ValidateEntryForSave over every entry of an
// agent's mcp_config, in both containers ("mcpServers" and the legacy
// "mcp"). The name of an OAuth entry there becomes part of its ref, so it
// must also match entryNamePattern. A document that does not parse is left to
// the caller.
func ValidateConfigForSave(ctx context.Context, raw json.RawMessage) error {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	for _, container := range []string{"mcpServers", "mcp"} {
		var servers map[string]json.RawMessage
		if json.Unmarshal(doc[container], &servers) != nil {
			continue
		}
		for name, entry := range servers {
			if err := ValidateEntryForSave(ctx, entry); err != nil {
				return fmt.Errorf("MCP server %q: %w", name, err)
			}
			if IsOAuthEntry(entry) && !ValidEntryName(name) {
				return fmt.Errorf("MCP server %q: %w: the name of an OAuth entry must be 1 to 64 letters, digits, '-' or '_'", name, ErrInvalidInput)
			}
		}
	}
	return nil
}

// ValidEntryName reports whether name can be part of an agent ref.
func ValidEntryName(name string) bool { return entryNamePattern.MatchString(name) }
