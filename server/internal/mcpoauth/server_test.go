// Added by the mouse9527/multica fork: manual OAuth endpoints, resource and audience for MCP entries (M-295 stage 2).
// Modified for the mouse9527/multica fork: extra authorization parameters (M-295 stage 3).

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func oauthEntryWith(auth string) json.RawMessage {
	return json.RawMessage(`{"type":"http","url":"https://mcp.example.com/mcp","auth":{"type":"oauth"` + auth + `}}`)
}

const manualAuth = `,"client_id":"c","issuer":"https://auth.example.com",` +
	`"authorization_endpoint":"https://auth.example.com/authorize?tenant=t","token_endpoint":"https://auth.example.com/token"`

func TestParseEntryServerSettings(t *testing.T) {
	entry, err := ParseEntry(oauthEntryWith(manualAuth + `,"resource":" https://api.example.com/v1 ","audience":"https://api.example.com"`))
	want := ServerConfig{
		AuthorizationEndpoint: "https://auth.example.com/authorize?tenant=t",
		TokenEndpoint:         "https://auth.example.com/token",
		Resource:              "https://api.example.com/v1",
		Audience:              "https://api.example.com",
	}
	if err != nil || entry.Server != want {
		t.Fatalf("server = %+v, err = %v", entry.Server, err)
	}
	for name, auth := range map[string]string{
		"urn resource":            `,"resource":"urn:example:mcp"`,
		"audience alone":          `,"audience":"my-api"`,
		"issuer alone":            `,"issuer":"https://auth.example.com"`,
		"issuer with resource":    `,"issuer":"https://auth.example.com","resource":"https://api.example.com"`,
		"manual with public auth": `,"client_id":"c","issuer":"https://auth.example.com","token_endpoint_auth_method":"none","authorization_endpoint":"https://a.example/authorize","token_endpoint":"https://a.example/token"`,
	} {
		if _, err := ParseEntry(oauthEntryWith(auth)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	invalid := map[string]string{
		"authorization endpoint alone": `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":"https://a.example/authorize"`,
		"token endpoint alone":         `,"client_id":"c","issuer":"https://a.example","token_endpoint":"https://a.example/token"`,
		"manual without client":        `,"authorization_endpoint":"https://a.example/authorize","token_endpoint":"https://a.example/token"`,
		"http endpoint":                `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":"http://a.example/authorize","token_endpoint":"https://a.example/token"`,
		"endpoint with fragment":       `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":"https://a.example/authorize#x","token_endpoint":"https://a.example/token"`,
		"endpoint with userinfo":       `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":"https://a.example/authorize","token_endpoint":"https://u:p@a.example/token"`,
		"endpoint not a string":        `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":1,"token_endpoint":"https://a.example/token"`,
		"relative resource":            `,"resource":"/mcp/resource"`,
		"resource with fragment":       `,"resource":"https://api.example.com/#x"`,
		"short resource":               `,"resource":"a:b"`,
		"resource with a space":        `,"resource":"https://api.example.com/a b"`,
		"empty audience":               `,"audience":"  "`,
		"audience with a newline":      `,"audience":"a\nb"`,
		"audience with a quote":        `,"audience":"a\"b"`,
		"too long audience":            `,"audience":"` + strings.Repeat("a", maxAudienceBytes+1) + `"`,
		"method without client":        `,"issuer":"https://a.example","token_endpoint_auth_method":"none"`,
	}
	for name, auth := range invalid {
		if _, err := ParseEntry(oauthEntryWith(auth)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

// Manual endpoints pass the same save-time address rules as the entry URL;
// they never bypass the SSRF boundary or the operator's allowlist.
func TestValidateEntryForSaveChecksManualEndpoints(t *testing.T) {
	ctx := context.Background()
	if err := ValidateEntryForSave(ctx, oauthEntryWith(manualAuth)); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	for _, endpoint := range []string{"https://127.0.0.1/token", "https://localhost/token", "https://10.0.0.1:8443/token?x=1", "https://[::1]/token"} {
		auth := `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":"https://a.example/authorize","token_endpoint":"` + endpoint + `"`
		if err := ValidateEntryForSave(ctx, oauthEntryWith(auth)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("token endpoint %s: err = %v, want ErrInvalidInput", endpoint, err)
		}
	}
	t.Setenv(AllowedOriginsEnv, "https://10.0.0.1:8443")
	auth := `,"client_id":"c","issuer":"https://a.example","authorization_endpoint":"https://10.0.0.1:8443/authorize?x=1","token_endpoint":"https://10.0.0.1:8443/token"`
	if err := ValidateEntryForSave(ctx, oauthEntryWith(auth)); err != nil {
		t.Fatalf("endpoints at an allowed origin: %v", err)
	}
}

func TestServerHostsAndAudience(t *testing.T) {
	auth, token := ServerConfig{AuthorizationEndpoint: "https://login.example.com:8443/authorize?x=1", TokenEndpoint: "https://api.example.com/token"}.Hosts()
	if auth != "login.example.com:8443" || token != "api.example.com" {
		t.Fatalf("hosts = %q, %q", auth, token)
	}
	if a, b := (ServerConfig{Resource: "https://api.example.com"}).Hosts(); a != "" || b != "" {
		t.Fatalf("hosts without manual endpoints = %q, %q", a, b)
	}

	got, err := withAuthorizationParams("https://auth.example.com/authorize?client_id=c&state=s", ServerConfig{Audience: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	if q := u.Query(); q.Get("audience") != "https://api.example.com" || q.Get("client_id") != "c" || q.Get("state") != "s" {
		t.Fatalf("with audience = %s", got)
	}
	if same, _ := withAuthorizationParams("https://auth.example.com/authorize?x=1", ServerConfig{}); same != "https://auth.example.com/authorize?x=1" {
		t.Fatalf("without audience = %s", same)
	}
}

func TestGrantMismatch(t *testing.T) {
	client := ClientConfig{ID: "c", Issuer: "https://a.example"}
	server := ServerConfig{AuthorizationEndpoint: "https://a.example/authorize", TokenEndpoint: "https://a.example/token", Resource: "https://api.example.com"}
	for _, c := range []struct {
		name                                      string
		clientID, method, tokenEndpoint, resource string
		want                                      string
	}{
		{"same", "c", AuthMethodNone, "https://a.example/token", "https://api.example.com", ""},
		{"other client", "d", AuthMethodNone, "https://a.example/token", "https://api.example.com", ReasonClientChanged},
		{"other token endpoint", "c", AuthMethodNone, "https://b.example/token", "https://api.example.com", ReasonServerChanged},
		{"other resource", "c", AuthMethodNone, "https://a.example/token", "https://mcp.example.com/mcp", ReasonServerChanged},
	} {
		if got := grantMismatch(client, server, c.clientID, c.method, c.tokenEndpoint, c.resource); got != c.want {
			t.Errorf("%s: grantMismatch = %q, want %q", c.name, got, c.want)
		}
	}
	// Without overrides, any token endpoint and resource the connect found.
	if got := grantMismatch(ClientConfig{}, ServerConfig{}, "x", AuthMethodNone, "https://any.example/token", "https://mcp.example.com/mcp"); got != "" {
		t.Fatalf("no overrides: %q", got)
	}
}
