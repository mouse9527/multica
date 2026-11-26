// Added by the mouse9527/multica fork: pre-registered OAuth clients for MCP entries (M-295).
// Modified for the mouse9527/multica fork: issuer alone, AuthorizationChange (M-295 stage 2).

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const preregisteredEntry = `{"type":"http","url":"https://mcp.example.com/mcp","auth":{"type":"oauth","scope":"mcp",` +
	`"client_id":"client-1","issuer":"https://auth.example.com/tenant","token_endpoint_auth_method":"client_secret_post"}}`

func TestParseEntryPreregisteredClient(t *testing.T) {
	entry, err := ParseEntry(json.RawMessage(preregisteredEntry))
	want := Entry{URL: "https://mcp.example.com/mcp", Scope: "mcp", Client: ClientConfig{
		ID: "client-1", Issuer: "https://auth.example.com/tenant", TokenEndpointAuthMethod: AuthMethodClientSecretPost,
	}}
	if err != nil || entry != want {
		t.Fatalf("entry = %+v, err = %v", entry, err)
	}
	for raw, want := range map[string]ClientConfig{
		// Without a client: dynamic registration, as before.
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth"}}`: {},
		// The method is optional; values are trimmed, not normalized.
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":" c ","issuer":" https://Auth.example.com/ "}}`: {
			ID: "c", Issuer: "https://Auth.example.com/",
		},
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://a.example","token_endpoint_auth_method":"none"}}`: {
			ID: "c", Issuer: "https://a.example", TokenEndpointAuthMethod: AuthMethodNone,
		},
		// An issuer alone: dynamic registration at a named server (stage 2).
		`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","issuer":"https://a.example"}}`: {Issuer: "https://a.example"},
	} {
		got, err := ParseEntry(json.RawMessage(raw))
		if err != nil || got.Client != want {
			t.Errorf("ParseEntry(%s).Client = %+v, %v; want %+v", raw, got.Client, err, want)
		}
	}

	invalid := map[string]string{
		"client secret in the entry": `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://a.example","client_secret":"s"}}`,
		"client id without issuer":   `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c"}}`,
		"method without client id":   `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","token_endpoint_auth_method":"none"}}`,
		"empty client id":            `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"  ","issuer":"https://a.example"}}`,
		"client id not a string":     `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":1,"issuer":"https://a.example"}}`,
		"client id with a newline":   `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"a\nb","issuer":"https://a.example"}}`,
		"client id not ASCII":        `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"客户端","issuer":"https://a.example"}}`,
		"unknown method":             `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://a.example","token_endpoint_auth_method":"private_key_jwt"}}`,
		"http issuer":                `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"http://a.example"}}`,
		"issuer with query":          `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://a.example/?x=1"}}`,
		"issuer with fragment":       `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://a.example/#x"}}`,
		"issuer with userinfo":       `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://u@a.example"}}`,
		"issuer not a string":        `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":true}}`,
		"too long client id":         `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"` + strings.Repeat("c", maxClientIDBytes+1) + `","issuer":"https://a.example"}}`,
	}
	for name, raw := range invalid {
		if _, err := ParseEntry(json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

// A client secret put into the entry is refused without echoing it.
func TestParseEntryRefusesClientSecretWithoutEchoingIt(t *testing.T) {
	_, err := ParseEntry(json.RawMessage(`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://a.example","client_secret":"sk-secret-value"}}`))
	if !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), "sk-secret-value") || !strings.Contains(err.Error(), "client_secret") {
		t.Fatalf("err = %v", err)
	}
}

// The issuer passes the same literal-address rules as the entry URL.
func TestValidateEntryForSaveChecksTheIssuer(t *testing.T) {
	ctx := context.Background()
	if err := ValidateEntryForSave(ctx, json.RawMessage(preregisteredEntry)); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	for _, issuer := range []string{"https://127.0.0.1", "https://localhost/x", "https://10.0.0.1:8443", "https://[::1]"} {
		raw := `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"` + issuer + `"}}`
		if err := ValidateEntryForSave(ctx, json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("issuer %s: err = %v, want ErrInvalidInput", issuer, err)
		}
	}
	t.Setenv(AllowedOriginsEnv, "https://10.0.0.1:8443")
	raw := `{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth","client_id":"c","issuer":"https://10.0.0.1:8443"}}`
	if err := ValidateEntryForSave(ctx, json.RawMessage(raw)); err != nil {
		t.Fatalf("issuer at an allowed origin: %v", err)
	}
}

func TestPreregisteredAuthMethod(t *testing.T) {
	for _, c := range []struct {
		explicit  string
		hasSecret bool
		supported []string
		want      string
		wantErr   error
	}{
		{"", false, nil, AuthMethodNone, nil},
		{"", false, []string{AuthMethodNone, AuthMethodClientSecretBasic}, AuthMethodNone, nil},
		// The server takes no public client and there is no secret: said
		// before the browser round trip, not at the code exchange.
		{"", false, []string{AuthMethodClientSecretBasic}, "", ErrClientSecretMissing},
		{"", true, nil, AuthMethodClientSecretBasic, nil},
		{"", true, []string{AuthMethodNone, AuthMethodClientSecretBasic, AuthMethodClientSecretPost}, AuthMethodClientSecretBasic, nil},
		{"", true, []string{AuthMethodClientSecretPost}, AuthMethodClientSecretPost, nil},
		{"", true, []string{"private_key_jwt"}, "", ErrUnsupportedClient},
		{AuthMethodClientSecretPost, true, []string{AuthMethodClientSecretBasic}, AuthMethodClientSecretPost, nil},
		{AuthMethodClientSecretBasic, false, nil, "", ErrClientSecretMissing},
		{AuthMethodClientSecretPost, false, nil, "", ErrClientSecretMissing},
		{AuthMethodNone, true, nil, AuthMethodNone, nil},
	} {
		got, err := preregisteredAuthMethod(c.explicit, c.hasSecret, c.supported)
		if got != c.want || (c.wantErr == nil) != (err == nil) || (c.wantErr != nil && !errors.Is(err, c.wantErr)) {
			t.Errorf("preregisteredAuthMethod(%q, %v, %v) = %q, %v; want %q, %v", c.explicit, c.hasSecret, c.supported, got, err, c.want, c.wantErr)
		}
	}
}

func TestClientMatchesAndChanges(t *testing.T) {
	dynamic := ClientConfig{}
	pre := ClientConfig{ID: "client-1", Issuer: "https://auth.example.com"}
	if !dynamic.clientMatches("whatever-was-registered", AuthMethodNone) {
		t.Error("a dynamically registered entry refused its grant")
	}
	if !pre.clientMatches("client-1", AuthMethodClientSecretBasic) || pre.clientMatches("client-2", AuthMethodClientSecretBasic) {
		t.Error("the client id is not what binds a grant to a pre-registered client")
	}
	pinned := ClientConfig{ID: "client-1", Issuer: "https://auth.example.com", TokenEndpointAuthMethod: AuthMethodClientSecretPost}
	if pinned.clientMatches("client-1", AuthMethodClientSecretBasic) || !pinned.clientMatches("client-1", AuthMethodClientSecretPost) {
		t.Error("an entry's explicit method is not what binds its grant")
	}

	entry := func(auth string) json.RawMessage {
		return json.RawMessage(`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth"` + auth + `}}`)
	}
	pre := `,"client_id":"c","issuer":"https://a.example"`
	manual := pre + `,"authorization_endpoint":"https://a.example/authorize","token_endpoint":"https://a.example/token"`
	for _, c := range []struct {
		name          string
		before, after json.RawMessage
		want          string
	}{
		{"unchanged dynamic", entry(``), entry(`,"scope":"other"`), ""},
		{"unchanged pre-registered", entry(pre), entry(pre + `,"scope":"x"`), ""},
		{"to pre-registered", entry(``), entry(pre), ReasonClientChanged},
		{"back to dynamic", entry(pre), entry(``), ReasonClientChanged},
		{"other method", entry(pre), entry(pre + `,"token_endpoint_auth_method":"none"`), ReasonClientChanged},
		{"other issuer", entry(pre), entry(`,"client_id":"c","issuer":"https://b.example"`), ReasonServerChanged},
		{"issuer named", entry(``), entry(`,"issuer":"https://a.example"`), ReasonServerChanged},
		{"manual endpoints", entry(pre), entry(manual), ReasonServerChanged},
		{"other token endpoint", entry(manual), entry(pre + `,"authorization_endpoint":"https://a.example/authorize","token_endpoint":"https://b.example/token"`), ReasonServerChanged},
		{"resource", entry(``), entry(`,"resource":"https://api.example.com"`), ReasonServerChanged},
		{"audience", entry(``), entry(`,"audience":"https://api.example.com"`), ReasonServerChanged},
		{"new entry", nil, entry(pre), ""},
		{"no longer OAuth", entry(pre), json.RawMessage(`{"url":"https://mcp.example.com/mcp"}`), ""},
	} {
		if got := AuthorizationChange(c.before, c.after); got != c.want {
			t.Errorf("%s: AuthorizationChange = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClientIdentityChanged(t *testing.T) {
	entry := func(auth string) json.RawMessage {
		return json.RawMessage(`{"url":"https://mcp.example.com/mcp","auth":{"type":"oauth"` + auth + `}}`)
	}
	pre := entry(`,"client_id":"c","issuer":"https://a.example"`)
	for _, c := range []struct {
		name  string
		after json.RawMessage
		want  bool
	}{
		{"scope only", entry(`,"client_id":"c","issuer":"https://a.example","scope":"x"`), false},
		{"method only", entry(`,"client_id":"c","issuer":"https://a.example","token_endpoint_auth_method":"client_secret_post"`), false},
		{"other issuer", entry(`,"client_id":"c","issuer":"https://b.example"`), true},
		{"other client id", entry(`,"client_id":"d","issuer":"https://a.example"`), true},
		{"back to dynamic", entry(``), true},
	} {
		if got := ClientIdentityChanged(pre, c.after); got != c.want {
			t.Errorf("%s: ClientIdentityChanged = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConnectionStatusForEntry(t *testing.T) {
	status := ConnectionStatus{Status: StatusConnected, ServerURL: "https://mcp.example.com/mcp", ClientID: "client-1", AuthMethod: AuthMethodClientSecretBasic}
	same := Entry{URL: "https://mcp.example.com/mcp", Client: ClientConfig{ID: "client-1", Issuer: "https://a.example"}}
	if got := status.ForEntry(same); got.Status != StatusConnected {
		t.Fatalf("same client: %+v", got)
	}
	other := same
	other.Client.ID = "client-2"
	if got := status.ForEntry(other); got.Status != StatusNeedsReconnect || got.LastError != ReasonClientChanged {
		t.Fatalf("other client: %+v", got)
	}
	moved := same
	moved.URL = "https://elsewhere.example.com/mcp"
	if got := status.ForEntry(moved); got.Status != StatusNeedsReconnect || got.LastError != ReasonURLChanged {
		t.Fatalf("other URL: %+v", got)
	}
}

func TestValidateClientSecret(t *testing.T) {
	for _, ok := range []string{"s", "a b~!@#$%^&*()", strings.Repeat("x", maxClientSecretBytes)} {
		if err := ValidateClientSecret(ok); err != nil {
			t.Errorf("ValidateClientSecret(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a\nb", "a\x00b", "密钥", strings.Repeat("x", maxClientSecretBytes+1)} {
		if err := ValidateClientSecret(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidateClientSecret(%q) = %v, want ErrInvalidInput", bad, err)
		}
	}
}
