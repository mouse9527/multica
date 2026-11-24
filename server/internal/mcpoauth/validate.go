// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ErrInvalidInput wraps every validation failure of a server definition.
var ErrInvalidInput = errors.New("invalid MCP OAuth server")

var (
	serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
)

const (
	maxExtraHeaders     = 8
	maxHeaderValueBytes = 4096
	maxScopeBytes       = 1024
)

// reservedHeaders are set by the MCP client or by this package. An extra
// header may not replace them: Authorization carries the OAuth token, and the
// rest belong to the transport.
var reservedHeaders = map[string]struct{}{
	"authorization":        {},
	"host":                 {},
	"content-type":         {},
	"content-length":       {},
	"accept":               {},
	"connection":           {},
	"transfer-encoding":    {},
	"mcp-session-id":       {},
	"mcp-protocol-version": {},
}

// ValidateServerURL checks the shape of an MCP server URL as stored. Whether
// its host may be reached is decided each time it is contacted, because DNS
// can change in between.
func ValidateServerURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 9 || len(raw) > 2048 {
		return nil, fmt.Errorf("%w: url must be 9 to 2048 characters", ErrInvalidInput)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: url must be HTTPS without userinfo, query or fragment", ErrInvalidInput)
	}
	return u, nil
}

func validateServerInput(in ServerInput) error {
	if !serverNamePattern.MatchString(in.Name) {
		return fmt.Errorf("%w: name must be 1 to 64 letters, digits, '-' or '_', starting with a letter or digit", ErrInvalidInput)
	}
	if _, err := ValidateServerURL(in.URL); err != nil {
		return err
	}
	if len(in.Scope) > maxScopeBytes || strings.ContainsAny(in.Scope, "\r\n\x00\"\\") {
		return fmt.Errorf("%w: scope is too long or has invalid characters", ErrInvalidInput)
	}
	return validateExtraHeaders(in.ExtraHeaders)
}

func validateExtraHeaders(headers map[string]string) error {
	if len(headers) > maxExtraHeaders {
		return fmt.Errorf("%w: at most %d extra headers", ErrInvalidInput, maxExtraHeaders)
	}
	seen := make(map[string]struct{}, len(headers))
	for name, value := range headers {
		if !headerNamePattern.MatchString(name) {
			return fmt.Errorf("%w: header name %q is not valid", ErrInvalidInput, name)
		}
		lower := strings.ToLower(name)
		if _, reserved := reservedHeaders[lower]; reserved {
			return fmt.Errorf("%w: header %q cannot be set here", ErrInvalidInput, name)
		}
		if _, dup := seen[lower]; dup {
			return fmt.Errorf("%w: header %q is given twice", ErrInvalidInput, name)
		}
		seen[lower] = struct{}{}
		// The value is never echoed back in an error: it is usually a key.
		if value == "" || len(value) > maxHeaderValueBytes || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%w: header %q value must be 1 to %d bytes on one line", ErrInvalidInput, name, maxHeaderValueBytes)
		}
	}
	return nil
}
