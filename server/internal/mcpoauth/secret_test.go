// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const leakProbe = "tok-4f1c9e-do-not-print"

// A token must not escape through any printed, logged or encoded form.
func TestSecretNeverPrintsItsValue(t *testing.T) {
	s := NewSecret(leakProbe)
	if s.Reveal() != leakProbe {
		t.Fatal("Reveal must return the value")
	}
	holder := struct {
		Token Secret
		Ptr   *Secret
	}{Token: s, Ptr: &s}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		if got := fmt.Sprintf(format, s); strings.Contains(got, leakProbe) {
			t.Errorf("%s printed the value: %s", format, got)
		}
		if got := fmt.Sprintf(format, holder); strings.Contains(got, leakProbe) {
			t.Errorf("%s of a struct printed the value: %s", format, got)
		}
	}
	raw, err := json.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), leakProbe) {
		t.Errorf("json printed the value: %s", raw)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("x", "token", s, "holder", holder)
	logger.Info("y", slog.Any("token", s))
	if strings.Contains(buf.String(), leakProbe) {
		t.Errorf("slog printed the value: %s", buf.String())
	}
}

func TestCredentialHeaders(t *testing.T) {
	c := Credential{
		AccessToken:  NewSecret("at-1"),
		ExtraHeaders: map[string]Secret{"x-gateway-key": NewSecret("k-1")},
	}
	got := c.Headers()
	if got["Authorization"] != "Bearer at-1" || got["x-gateway-key"] != "k-1" || len(got) != 2 {
		t.Fatalf("headers = %v", got)
	}
	if strings.Contains(fmt.Sprintf("%v %+v", c, c), "at-1") {
		t.Fatal("a printed Credential shows the token")
	}
}

func TestNewVerifierIsPKCEShaped(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		v, err := NewVerifier()
		if err != nil {
			t.Fatal(err)
		}
		value := v.Reveal()
		// RFC 7636: 43 to 128 characters from [A-Za-z0-9-._~].
		if len(value) < 43 || len(value) > 128 || strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~") != "" {
			t.Fatalf("verifier %q is not a PKCE verifier", value)
		}
		if seen[value] {
			t.Fatal("verifier repeated")
		}
		seen[value] = true
	}
}
