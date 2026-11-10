package githubissues

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeToken is shaped like a classic PAT but is not a real credential.
const fakeToken = "ghp_FAKEtokenFORtestsONLY0123456789abcd"

func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoadConfigOffByDefault(t *testing.T) {
	cfg := LoadConfig(envOf(nil))
	if cfg.Enabled() {
		t.Fatal("feature must be off when no variable is set")
	}
	if !strings.Contains(cfg.DisabledReason(), EnvEnabled) {
		t.Fatalf("reason %q should name %s", cfg.DisabledReason(), EnvEnabled)
	}
}

// Only "true" turns the feature on, like MULTICA_VCS_INTEGRATION_ENABLED.
func TestLoadConfigSwitchOffIgnoresToken(t *testing.T) {
	for _, value := range []string{"", "false", "1", "TRUE", "yes"} {
		cfg := LoadConfig(envOf(map[string]string{
			EnvEnabled:        value,
			EnvToken:          fakeToken,
			EnvTokenExpiresAt: "never",
		}))
		if cfg.Enabled() {
			t.Errorf("%s=%q must keep the feature off", EnvEnabled, value)
		}
		if cfg.token != "" {
			t.Errorf("%s=%q: token must not be retained while off", EnvEnabled, value)
		}
	}
}

func TestLoadConfigMissingTokenDisablesWithoutError(t *testing.T) {
	for _, token := range []string{"", "   "} {
		cfg := LoadConfig(envOf(map[string]string{
			EnvEnabled:        "true",
			EnvToken:          token,
			EnvTokenExpiresAt: "2027-01-31",
		}))
		if cfg.Enabled() {
			t.Fatalf("token %q: feature must be off", token)
		}
		if cfg.DisabledReason() != EnvToken+" is not set" {
			t.Fatalf("token %q: reason = %q", token, cfg.DisabledReason())
		}
	}
}

func TestLoadConfigExpiry(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		enabled    bool
		wantExpiry time.Time
		wantOK     bool
	}{
		{name: "date", raw: "2027-01-31", enabled: true, wantExpiry: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), wantOK: true},
		{name: "date with spaces", raw: " 2027-01-31 ", enabled: true, wantExpiry: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), wantOK: true},
		{name: "never", raw: "never", enabled: true},
		{name: "never any case", raw: "Never", enabled: true},
		{name: "missing", raw: ""},
		{name: "not a date", raw: "soon"},
		{name: "impossible date", raw: "2027-02-30"},
		{name: "timestamp", raw: "2027-01-31T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := LoadConfig(envOf(map[string]string{
				EnvEnabled:        "true",
				EnvToken:          fakeToken,
				EnvTokenExpiresAt: tc.raw,
			}))
			if cfg.Enabled() != tc.enabled {
				t.Fatalf("Enabled() = %v, want %v (reason %q)", cfg.Enabled(), tc.enabled, cfg.DisabledReason())
			}
			if !tc.enabled {
				if !strings.Contains(cfg.DisabledReason(), EnvTokenExpiresAt) {
					t.Fatalf("reason %q should name %s", cfg.DisabledReason(), EnvTokenExpiresAt)
				}
				if tc.raw != "" && strings.Contains(cfg.DisabledReason(), strings.TrimSpace(tc.raw)) {
					t.Fatalf("reason %q must not echo the variable's value", cfg.DisabledReason())
				}
				return
			}
			if cfg.token != fakeToken {
				t.Fatal("enabled config must keep the token")
			}
			gotExpiry, gotOK := cfg.TokenExpiresAt()
			if gotOK != tc.wantOK || !gotExpiry.Equal(tc.wantExpiry) {
				t.Fatalf("TokenExpiresAt() = %v, %v; want %v, %v", gotExpiry, gotOK, tc.wantExpiry, tc.wantOK)
			}
		})
	}
}

// A token pasted into the expiry variable by mistake must not be echoed.
func TestLoadConfigDoesNotEchoTokenPastedIntoExpiry(t *testing.T) {
	cfg := LoadConfig(envOf(map[string]string{
		EnvEnabled:        "true",
		EnvToken:          fakeToken,
		EnvTokenExpiresAt: fakeToken,
	}))
	if cfg.Enabled() {
		t.Fatal("a token is not an expiry date")
	}
	assertNoToken(t, "DisabledReason", cfg.DisabledReason())
	assertRenderingsHideToken(t, cfg)
}

func TestEnabledConfigNeverRendersToken(t *testing.T) {
	cfg := LoadConfig(envOf(map[string]string{
		EnvEnabled:        "true",
		EnvToken:          fakeToken,
		EnvTokenExpiresAt: "2027-01-31",
	}))
	if !cfg.Enabled() {
		t.Fatalf("expected enabled config, reason %q", cfg.DisabledReason())
	}
	assertRenderingsHideToken(t, cfg)
	if !strings.Contains(cfg.String(), "2027-01-31") || !strings.Contains(cfg.String(), redacted) {
		t.Fatalf("String() = %q, want expiry and redaction marker", cfg.String())
	}
}

func TestLogStatusNeverLogsToken(t *testing.T) {
	for _, env := range []map[string]string{
		{EnvEnabled: "true", EnvToken: fakeToken, EnvTokenExpiresAt: "never"},
		{EnvEnabled: "true", EnvToken: fakeToken, EnvTokenExpiresAt: "bad"},
		{EnvEnabled: "false", EnvToken: fakeToken, EnvTokenExpiresAt: "never"},
	} {
		for _, newHandler := range []func(*bytes.Buffer) slog.Handler{
			func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var buf bytes.Buffer
			LoadConfig(envOf(env)).LogStatus(slog.New(newHandler(&buf)))
			if buf.Len() == 0 {
				t.Fatal("LogStatus wrote nothing")
			}
			assertNoToken(t, "log line", buf.String())
		}
	}
}

func assertRenderingsHideToken(t *testing.T, cfg Config) {
	t.Helper()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		assertNoToken(t, verb, fmt.Sprintf(verb, cfg))
		assertNoToken(t, verb+" (pointer)", fmt.Sprintf(verb, &cfg))
		assertNoToken(t, verb+" (nested)", fmt.Sprintf(verb, struct{ C Config }{cfg}))
	}
	wrapped := fmt.Errorf("start github issues with %v: %w", cfg, errors.New("boom"))
	assertNoToken(t, "error", wrapped.Error())

	encoded, err := json.Marshal(map[string]any{"config": cfg, "pointer": &cfg})
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, "json", string(encoded))

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", cfg, "pointer", &cfg)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", "config", cfg, "any", any(cfg))
	assertNoToken(t, "slog", buf.String())
}

func assertNoToken(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, fakeToken) {
		t.Fatalf("%s leaks the token: %s", where, s)
	}
	// A prefix is enough to identify a token, so partial leaks fail too.
	if strings.Contains(s, fakeToken[:12]) {
		t.Fatalf("%s leaks part of the token: %s", where, s)
	}
}
