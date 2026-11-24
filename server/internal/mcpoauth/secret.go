// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"fmt"
	"log/slog"
)

const redacted = "[REDACTED]"

// Secret holds a credential value. Every printed, logged or JSON form is
// redacted; Reveal is the only way to read it, so a stray %v, slog attribute
// or json.Marshal cannot put a token in a log line or a response.
type Secret struct{ value string }

func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the value. Call it only where the value leaves for its
// intended recipient (a request header, the claim response).
func (s Secret) Reveal() string { return s.value }

func (s Secret) Empty() bool { return s.value == "" }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format makes every fmt verb, including %#v and %q, print the placeholder.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
