// Added by the mouse9527/multica fork: extra authorization request parameters for MCP entries (M-295 stage 3).

package mcpoauth

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParseEntryAuthorizationParams(t *testing.T) {
	entry, err := ParseEntry(oauthEntryWith(`,"authorization_params":{"prompt":"consent","access_type":" offline ","login_hint":"a@example.com",` +
		`"display":"page","ui_locales":"zh-CN en","hd":"example.com"}`))
	want := AuthorizationParams{Prompt: "consent", AccessType: "offline", LoginHint: "a@example.com", Display: "page", UILocales: "zh-CN en", HD: "example.com"}
	if err != nil || entry.Server.Params != want {
		t.Fatalf("params = %+v, err = %v", entry.Server.Params, err)
	}
	if entry, err := ParseEntry(oauthEntryWith(`,"authorization_params":{}`)); err != nil || entry.Server.Params != (AuthorizationParams{}) {
		t.Fatalf("empty params = %+v, %v", entry.Server.Params, err)
	}

	for name, params := range map[string]string{
		"not an object":       `"prompt=consent"`,
		"null":                `null`,
		"unknown key":         `{"max_age":"0"}`,
		"value not a string":  `{"prompt":1}`,
		"empty value":         `{"prompt":"  "}`,
		"quote":               `{"login_hint":"a\"b"}`,
		"backslash":           `{"login_hint":"a\\b"}`,
		"newline":             `{"login_hint":"a\nb"}`,
		"too long":            `{"login_hint":"` + strings.Repeat("a", maxScopeBytes+1) + `"}`,
		"protocol: state":     `{"state":"x"}`,
		"protocol: client_id": `{"client_id":"x"}`,
		"protocol: scope":     `{"scope":"x"}`,
		"protocol: resource":  `{"resource":"https://x.example"}`,
		"protocol: audience":  `{"audience":"x"}`,
		"protocol: redirect":  `{"redirect_uri":"https://evil.example"}`,
		"protocol: pkce":      `{"code_challenge_method":"plain"}`,
		"protocol: response":  `{"response_type":"token"}`,
	} {
		if _, err := ParseEntry(oauthEntryWith(`,"authorization_params":` + params)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}

	// An unknown key is not echoed: it is whatever the entry's author typed.
	_, err = ParseEntry(oauthEntryWith(`,"authorization_params":{"sk-secret-looking-key":"x"}`))
	if err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("err = %v", err)
	}
	// A protocol key is named, so the author knows why.
	_, err = ParseEntry(oauthEntryWith(`,"authorization_params":{"redirect_uri":"https://evil.example"}`))
	if err == nil || !strings.Contains(err.Error(), "redirect_uri") || strings.Contains(err.Error(), "evil") {
		t.Fatalf("err = %v", err)
	}
}

func TestWithAuthorizationParams(t *testing.T) {
	server := ServerConfig{Audience: "my-api", Params: AuthorizationParams{Prompt: "consent", AccessType: "offline", HD: "example.com"}}
	got, err := withAuthorizationParams("https://auth.example.com/authorize?tenant=t&client_id=c&state=s", server)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	q := u.Query()
	for key, want := range map[string]string{
		"tenant": "t", "client_id": "c", "state": "s", "audience": "my-api", "prompt": "consent", "access_type": "offline", "hd": "example.com",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q (%s)", key, q.Get(key), want, got)
		}
	}
	if q.Has("login_hint") || q.Has("display") || q.Has("ui_locales") {
		t.Fatalf("unset parameters were added: %s", got)
	}
}

// The extra parameters shape the next authorization; a grant keeps what it
// was given, so changing them alone does not take it out of use.
func TestAuthorizationChangeIgnoresParams(t *testing.T) {
	before := oauthEntryWith(`,"authorization_params":{"prompt":"consent"}`)
	for _, after := range []json.RawMessage{
		oauthEntryWith(`,"authorization_params":{"prompt":"login","access_type":"offline"}`),
		oauthEntryWith(``),
	} {
		if got := AuthorizationChange(before, after); got != "" {
			t.Errorf("AuthorizationChange(%s, %s) = %q, want none", before, after, got)
		}
	}
	if got := AuthorizationChange(before, oauthEntryWith(`,"authorization_params":{"prompt":"consent"},"audience":"x"`)); got != ReasonServerChanged {
		t.Fatalf("audience change = %q", got)
	}
}
