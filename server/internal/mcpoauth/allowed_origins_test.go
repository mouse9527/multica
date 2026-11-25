// Added by the mouse9527/multica fork: parse the allowed origins once per value (M-279).

package mcpoauth

import (
	"bytes"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
)

// An invalid value is parsed and reported once, however many calls read it;
// a changed value is parsed again.
func TestAllowedOriginsInvalidValueWarnsOnce(t *testing.T) {
	allowedOriginsCache.Lock()
	allowedOriginsCache.parsed, allowedOriginsCache.raw = false, ""
	allowedOriginsCache.Unlock()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	warnings := func() int { return strings.Count(logs.String(), AllowedOriginsEnv+" is invalid") }
	call := func(times int) {
		t.Helper()
		for range times {
			if _, ok := allowedOrigins(logger); ok {
				t.Fatalf("%s=%q was accepted", AllowedOriginsEnv, os.Getenv(AllowedOriginsEnv))
			}
		}
	}

	t.Setenv(AllowedOriginsEnv, "http://10.0.0.5")
	call(5)
	if n := warnings(); n != 1 {
		t.Fatalf("warnings after 5 calls with one invalid value = %d, want 1", n)
	}

	t.Setenv(AllowedOriginsEnv, "https://10.0.0.5,https://*.example.com")
	call(3)
	if n := warnings(); n != 2 {
		t.Fatalf("warnings after the value changed = %d, want 2", n)
	}

	t.Setenv(AllowedOriginsEnv, "https://10.0.0.5")
	origins, ok := allowedOrigins(logger)
	if !ok || !origins.Allows(&url.URL{Scheme: "https", Host: "10.0.0.5"}) || origins.Allows(&url.URL{Scheme: "https", Host: "10.0.0.6"}) {
		t.Fatalf("valid value: ok = %v, origins = %+v", ok, origins)
	}
	if n := warnings(); n != 2 {
		t.Fatalf("a valid value logged a warning: %d warnings, want 2", n)
	}

	// Back to the first invalid value: it changed, so it is parsed and
	// reported again.
	t.Setenv(AllowedOriginsEnv, "http://10.0.0.5")
	call(2)
	if n := warnings(); n != 3 {
		t.Fatalf("warnings after returning to an invalid value = %d, want 3", n)
	}
}
