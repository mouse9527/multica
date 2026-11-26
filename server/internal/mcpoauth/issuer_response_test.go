// Added by the mouse9527/multica fork: RFC 9207 authorization response issuer validation (M-295 stage 2).

package mcpoauth

import (
	"errors"
	"strings"
	"testing"
)

func TestStateRecordsTheIssuer(t *testing.T) {
	const random = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	for _, c := range []struct {
		issuer   string
		required bool
	}{
		{"https://auth.example.com", true},
		{"https://auth.example.com/tenant/", false},
		{"https://Auth.Example.com:443/a.b", true},
	} {
		state := stateWithIssuer(random, c.issuer, c.required)
		if !strings.HasPrefix(state, random+".") {
			t.Fatalf("state %q does not start with its random part", state)
		}
		if issuer, required := issuerFromState(state); issuer != c.issuer || required != c.required {
			t.Fatalf("issuerFromState(%q) = %q, %v; want %q, %v", state, issuer, required, c.issuer, c.required)
		}
	}
	if state := stateWithIssuer(random, "", true); state != random {
		t.Fatalf("state without an issuer = %q", state)
	}
	for _, state := range []string{random, random + ".", random + ".x" + "aHR0cHM", random + ".1***", ""} {
		if issuer, required := issuerFromState(state); issuer != "" || required {
			t.Errorf("issuerFromState(%q) = %q, %v; want nothing", state, issuer, required)
		}
	}
}

// The MCP authorization spec's table for RFC 9207 §2.4.
func TestCheckResponseIssuer(t *testing.T) {
	const issuer = "https://auth.example.com"
	for _, c := range []struct {
		name     string
		required bool
		iss      string
		ok       bool
	}{
		{"advertised, present, same", true, issuer, true},
		{"advertised, present, other", true, "https://evil.example.com", false},
		{"advertised, absent", true, "", false},
		{"not advertised, present, same", false, issuer, true},
		{"not advertised, present, other", false, "https://evil.example.com", false},
		{"not advertised, absent", false, "", true},
		// Simple string comparison: no normalization.
		{"trailing slash", true, issuer + "/", false},
		{"case", true, "https://Auth.example.com", false},
		{"default port", true, "https://auth.example.com:443", false},
	} {
		err := checkResponseIssuer(PendingAuthorization{Issuer: issuer, ISSRequired: c.required}, c.iss)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrResponseIssuer)) {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
	// A connect that recorded no issuer (started before this check) has
	// nothing to compare against.
	if err := checkResponseIssuer(PendingAuthorization{}, "https://any.example.com"); err != nil {
		t.Fatalf("no recorded issuer: %v", err)
	}
}
