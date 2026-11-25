// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

const validEntry = `{"type":"http","url":"https://mcp.example.com/mcp","auth":{"type":"oauth","scope":"mcp read"}}`

func TestParseEntry(t *testing.T) {
	entry, err := ParseEntry(json.RawMessage(validEntry))
	if err != nil || entry.URL != "https://mcp.example.com/mcp" || entry.Scope != "mcp read" || entry.Disabled {
		t.Fatalf("entry = %+v, err = %v", entry, err)
	}
	for raw, want := range map[string]Entry{
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth"}}`:                                    {URL: "https://mcp.example.com/mcp"},
		`{"type":"streamable-http","url":" https://mcp.example.com/mcp ","auth":{"type":"oauth"}}`:         {URL: "https://mcp.example.com/mcp"},
		`{"url":"https://mcp.example.com/mcp","headers":{},"auth":{"type":"oauth"}}`:                       {URL: "https://mcp.example.com/mcp"},
		`{"url":"https://mcp.example.com/mcp","enabled":false,"auth":{"type":"oauth"}}`:                    {URL: "https://mcp.example.com/mcp", Disabled: true},
		`{"url":"https://mcp.example.com/mcp","disabled":true,"auth":{"type":"oauth"}}`:                    {URL: "https://mcp.example.com/mcp", Disabled: true},
		`{"url":"https://mcp.example.com/mcp","disabled":false,"enabled":true,"auth":{"type":"oauth"}}`:    {URL: "https://mcp.example.com/mcp"},
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","scope":"  offline_access  "},"x":1}`: {URL: "https://mcp.example.com/mcp", Scope: "offline_access"},
	} {
		got, err := ParseEntry(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("ParseEntry(%s) = %+v, %v; want %+v", raw, got, err, want)
		}
	}

	invalid := map[string]string{
		"not an object":         `[1]`,
		"auth not an object":    `{"url":"https://mcp.example.com/mcp","auth":"oauth"}`,
		"other auth type":       `{"url":"https://mcp.example.com/mcp","auth":{"type":"bearer"}}`,
		"auth type in capitals": `{"url":"https://mcp.example.com/mcp","auth":{"type":"OAuth"}}`,
		"unknown auth field":    `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_secret":"s"}}`,
		"scope not a string":    `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","scope":1}}`,
		"scope with a newline":  `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","scope":"a\nb"}}`,
		"stdio":                 `{"command":"npx","url":"https://mcp.example.com/mcp","auth":{"type":"oauth"}}`,
		"sse":                   `{"type":"sse","url":"https://mcp.example.com/mcp","auth":{"type":"oauth"}}`,
		"headers":               `{"url":"https://mcp.example.com/mcp","headers":{"x-api-key":"k"},"auth":{"type":"oauth"}}`,
		"no url":                `{"type":"http","auth":{"type":"oauth"}}`,
		"http url":              `{"url":"http://mcp.example.com/mcp","auth":{"type":"oauth"}}`,
		"url with userinfo":     `{"url":"https://u:p@mcp.example.com/mcp","auth":{"type":"oauth"}}`,
		"url with query":        `{"url":"https://mcp.example.com/mcp?k=v","auth":{"type":"oauth"}}`,
		"url with fragment":     `{"url":"https://mcp.example.com/mcp#x","auth":{"type":"oauth"}}`,
		"url without host":      `{"url":"https:///mcp","auth":{"type":"oauth"}}`,
	}
	for name, raw := range invalid {
		if _, err := ParseEntry(json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

// A header value is never repeated in an error: it is usually a key.
func TestParseEntryDoesNotEchoValues(t *testing.T) {
	_, err := ParseEntry(json.RawMessage(`{"url":"https://mcp.example.com/mcp","headers":{"x-api-key":"sk-secret-value"},"auth":{"type":"oauth"}}`))
	if err == nil || strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("err = %v", err)
	}
}

// Detection is looser than parsing, so an entry that only nearly asks for
// OAuth is still withheld rather than mounted without a token.
func TestIsOAuthEntry(t *testing.T) {
	for raw, want := range map[string]bool{
		validEntry: true,
		`{"url":"https://a.example/mcp","auth":{"type":"OAuth"}}`:  true,
		`{"url":"https://a.example/mcp","auth":{"type":" oauth"}}`: true,
		`{"command":"npx","auth":{"type":"oauth"}}`:                true,
		`{"url":"https://a.example/mcp"}`:                          false,
		`{"url":"https://a.example/mcp","auth":{"type":"bearer"}}`: false,
		`{"url":"https://a.example/mcp","auth":"oauth"}`:           false,
		`{"url":"https://a.example/mcp","auth":{"type":1}}`:        false,
		`not json`: false,
	} {
		if got := IsOAuthEntry(json.RawMessage(raw)); got != want {
			t.Errorf("IsOAuthEntry(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestRefs(t *testing.T) {
	const id = "0b6f1b8e-5d0a-4c8e-9d61-2f0d7c3a1e55"
	if ref := WorkspaceRef(id); ref != "ws:"+id {
		t.Fatalf("WorkspaceRef = %q", ref)
	}
	if ref := AgentRef(id, "gateway"); ref != "agent:"+id+":gateway" || !strings.HasPrefix(ref, AgentRefPrefix(id)) {
		t.Fatalf("AgentRef = %q", ref)
	}
	if got, ok := ParseRef("ws:" + id); !ok || got != (Ref{WorkspaceServerID: id}) {
		t.Fatalf("ParseRef(ws) = %+v, %v", got, ok)
	}
	if got, ok := ParseRef("agent:" + id + ":my_gateway-2"); !ok || got != (Ref{AgentID: id, Name: "my_gateway-2"}) {
		t.Fatalf("ParseRef(agent) = %+v, %v", got, ok)
	}
	for _, ref := range []string{
		"", "ws:", "ws:not-a-uuid", "ws:" + strings.ToUpper(id), "agent:" + id, "agent:" + id + ":", "agent:" + id + ":bad name",
		"agent:" + id + ":a/b", "agent:x:gateway", "agent:" + id + ":" + strings.Repeat("a", 65), "server:" + id,
	} {
		if _, ok := ParseRef(ref); ok {
			t.Errorf("ParseRef(%q) accepted", ref)
		}
	}
}

func TestEntryURLAllowed(t *testing.T) {
	ctx := context.Background()
	t.Setenv(remotemcp.DevOriginsEnv, "")
	t.Setenv(AllowedOriginsEnv, "")
	for _, url := range []string{"https://mcp.example.com/mcp", "https://litellm.home.example/mcp/", "https://1.1.1.1/mcp"} {
		if err := EntryURLAllowed(ctx, url); err != nil {
			t.Errorf("%s: %v", url, err)
		}
	}
	refused := []string{
		"http://mcp.example.com/mcp", "https://localhost/mcp", "https://mcp.localhost/mcp", "https://127.0.0.1/mcp",
		"https://10.0.0.5/mcp", "https://192.168.1.10:8443/mcp", "https://169.254.169.254/latest", "https://[::1]/mcp",
	}
	for _, url := range refused {
		if err := EntryURLAllowed(ctx, url); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", url, err)
		}
	}
	// The operator's exact origin lets one private address through, and
	// only that one.
	t.Setenv(AllowedOriginsEnv, "https://10.0.0.5")
	if err := EntryURLAllowed(ctx, "https://10.0.0.5/mcp"); err != nil {
		t.Fatalf("allowed origin: %v", err)
	}
	if err := EntryURLAllowed(ctx, "https://10.0.0.6/mcp"); err == nil {
		t.Fatal("an origin next to the allowed one passed")
	}
}

func TestValidateForSave(t *testing.T) {
	ctx := context.Background()
	t.Setenv(remotemcp.DevOriginsEnv, "")
	t.Setenv(AllowedOriginsEnv, "")

	// Entries that do not declare auth are not this package's business.
	for _, raw := range []string{`{"command":"npx"}`, `{"url":"http://anything"}`, `[1]`} {
		if err := ValidateEntryForSave(ctx, json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if err := ValidateEntryForSave(ctx, json.RawMessage(validEntry)); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	for _, raw := range []string{
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"none"}}`,
		`{"url":"https://127.0.0.1/mcp","auth":{"type":"oauth"}}`,
		`{"url":"https://mcp.example.com/mcp","headers":{"Authorization":"Bearer x"},"auth":{"type":"oauth"}}`,
	} {
		if err := ValidateEntryForSave(ctx, json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", raw, err)
		}
	}

	ok := `{"mcpServers":{"plain":{"command":"npx"},"gateway":` + validEntry + `},"mcp":{"legacy":{"url":"https://x.example"}}}`
	if err := ValidateConfigForSave(ctx, json.RawMessage(ok)); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, raw := range map[string]string{
		"bad entry in mcpServers": `{"mcpServers":{"gateway":{"url":"http://x.example","auth":{"type":"oauth"}}}}`,
		"bad entry in legacy mcp": `{"mcp":{"gateway":{"url":"http://x.example","auth":{"type":"oauth"}}}}`,
		"name not usable in ref":  `{"mcpServers":{"my gateway":` + validEntry + `}}`,
		"name too long":           `{"mcpServers":{"` + strings.Repeat("g", 65) + `":` + validEntry + `}}`,
	} {
		if err := ValidateConfigForSave(ctx, json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
	// A plain entry may keep any name upstream allows.
	if err := ValidateConfigForSave(ctx, json.RawMessage(`{"mcpServers":{"my server":{"command":"npx"}}}`)); err != nil {
		t.Fatalf("plain entry with a free name: %v", err)
	}
}
