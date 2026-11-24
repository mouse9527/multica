// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package remotemcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseAllowedOrigins(t *testing.T) {
	origins, err := ParseAllowedOrigins(" https://MCP.Example.com:443 , https://gw.lan.test:1443/ ,")
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]bool{
		"https://mcp.example.com/mcp":                true,
		"https://mcp.example.com.:443/x":             true,
		"https://gw.lan.test:1443/mcp":               true,
		"https://gw.lan.test/mcp":                    false, // other port
		"http://gw.lan.test:1443/mcp":                false, // not HTTPS
		"https://gw.lan.test:1443.evil.test/":        false,
		"https://evil.test/https://gw.lan.test:1443": false,
	} {
		u, _ := url.Parse(raw)
		if got := origins.Allows(u); got != want {
			t.Errorf("Allows(%s) = %v, want %v", raw, got, want)
		}
	}
	for _, bad := range []string{"http://gw.lan.test", "https://gw.lan.test/mcp", "https://*.home.test", "https://u@gw.lan.test", "gw.lan.test", "https://gw.lan.test?x=1"} {
		if _, err := ParseAllowedOrigins(bad); err == nil {
			t.Errorf("ParseAllowedOrigins(%q) accepted", bad)
		}
	}
	// One bad entry fails the whole value: nothing is allowed by accident.
	if origins, err := ParseAllowedOrigins("https://ok.test, http://bad.test"); err == nil || origins.Len() != 0 {
		t.Fatalf("mixed value: %v, %d origins", err, origins.Len())
	}
}

// Only a call whose context carries the origin may reach a non-public
// address, and only that exact origin over HTTPS.
func TestValidatePublicHTTPSEndpointHonorsContextOrigins(t *testing.T) {
	origins, err := ParseAllowedOrigins("https://127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	allowed := WithAllowedOrigins(context.Background(), origins)
	if _, err := ValidatePublicHTTPSEndpoint(allowed, "https://127.0.0.1:8443/mcp", nil, nil); err != nil {
		t.Fatalf("allowed origin refused: %v", err)
	}
	for _, raw := range []string{"https://127.0.0.1:9443/mcp", "http://127.0.0.1:8443/mcp", "https://127.0.0.1:8443/mcp?x=1"} {
		if _, err := ValidatePublicHTTPSEndpoint(allowed, raw, nil, nil); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	if _, err := ValidatePublicHTTPSEndpoint(context.Background(), "https://127.0.0.1:8443/mcp", nil, nil); err == nil {
		t.Fatal("a non-public address was accepted without the origin in the context")
	}
	// The host policy still applies on top of the origin.
	if _, err := ValidatePublicHTTPSEndpoint(allowed, "https://127.0.0.1:8443/mcp", []string{"other.example"}, nil); err == nil {
		t.Fatal("the host policy was bypassed")
	}
}

// The dialer applies the same rule: with the origin in the request context
// the connection reaches the server (and then fails certificate verification
// against the system roots, which still apply); without it the address is
// refused before connecting.
func TestSecureClientDialHonorsContextOrigins(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	endpoint, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	origins, err := ParseAllowedOrigins(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	call := func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := NewSecureHTTPClient(endpoint).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := call(context.Background()); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("without the origin: err = %v, want a non-public refusal", err)
	}
	err = call(WithAllowedOrigins(context.Background(), origins))
	if err == nil || strings.Contains(err.Error(), "non-public") || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("with the origin: err = %v, want a certificate error from a real connection", err)
	}
}
