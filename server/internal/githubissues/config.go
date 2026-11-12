package githubissues

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvEnabled is the feature switch. Anything other than "true" keeps the
	// feature off.
	EnvEnabled = "MULTICA_GITHUB_ISSUES_ENABLED"
	// EnvToken holds the classic personal access token (scope `repo`).
	EnvToken = "MULTICA_GITHUB_TOKEN"
	// EnvTokenExpiresAt is the token's expiry date as registered on GitHub:
	// YYYY-MM-DD, or "never" for a token created without an expiry.
	EnvTokenExpiresAt = "MULTICA_GITHUB_TOKEN_EXPIRES_AT"
	// EnvSyncAccount names the Multica account recorded as the creator of
	// issues opened by other people on GitHub: "member:<user-uuid>" or
	// "agent:<agent-uuid>". Unset selects each workspace's first owner.
	EnvSyncAccount = "MULTICA_GITHUB_SYNC_ACCOUNT"
	// EnvPollEnabled turns the scheduled poll on. Anything other than "true"
	// leaves it off: GitHub is then read only when someone asks for it
	// (POST /api/issues/{id}/github-sync, multica issue sync).
	EnvPollEnabled = "MULTICA_GITHUB_POLL_ENABLED"
	// EnvRefreshOnView turns the refresh of a GitHub issue on opening it on.
	// Anything other than "true" leaves it off.
	EnvRefreshOnView = "MULTICA_GITHUB_REFRESH_ON_VIEW"
	// EnvSyncMinRemaining is the quota guard of manual syncs: below this many
	// requests left in GitHub's hourly quota they are refused (default
	// defaultSyncMinRemaining).
	EnvSyncMinRemaining = "MULTICA_GITHUB_SYNC_MIN_REMAINING"

	defaultSyncMinRemaining = 1000

	tokenNeverExpires = "never"
	expiryDateLayout  = time.DateOnly
	redacted          = "[REDACTED]"
)

// Config is the deployment-level configuration of GitHub-native issues, read
// once from the environment at startup.
//
// The token is held unexported and never leaves this package: Config renders
// itself without it under fmt, slog and encoding/json, so passing a Config to
// a logger or an error cannot leak the token.
type Config struct {
	enabled        bool
	disabledReason string
	token          string
	// expiresAt is midnight UTC of the expiry date; zero when the token never
	// expires.
	expiresAt time.Time
	// syncAccount is the configured creator of external issues; zero when
	// unset.
	syncAccount SyncAccount
	// pollEnabled and refreshOnView are the switches of the background
	// reads; both off unless set to "true".
	pollEnabled   bool
	refreshOnView bool
	// syncMinRemaining is the quota guard threshold of manual syncs.
	syncMinRemaining int
}

// LoadConfig reads the configuration through getenv (os.Getenv in
// production). It never fails: a missing or malformed variable turns the
// feature off and is described by DisabledReason, which names variables but
// never echoes their values.
func LoadConfig(getenv func(string) string) Config {
	if strings.TrimSpace(getenv(EnvEnabled)) != "true" {
		return disabled(EnvEnabled + " is not true")
	}
	token := strings.TrimSpace(getenv(EnvToken))
	if token == "" {
		return disabled(EnvToken + " is not set")
	}
	rawExpiry := strings.TrimSpace(getenv(EnvTokenExpiresAt))
	if rawExpiry == "" {
		return disabled(EnvTokenExpiresAt + " is not set (use YYYY-MM-DD or never)")
	}
	cfg := Config{
		enabled: true, token: token,
		pollEnabled:      strings.TrimSpace(getenv(EnvPollEnabled)) == "true",
		refreshOnView:    strings.TrimSpace(getenv(EnvRefreshOnView)) == "true",
		syncMinRemaining: defaultSyncMinRemaining,
	}
	if !strings.EqualFold(rawExpiry, tokenNeverExpires) {
		// A misplaced token in this variable must not reach the log, so the
		// parse error (which quotes its input) is dropped.
		expiresAt, err := time.ParseInLocation(expiryDateLayout, rawExpiry, time.UTC)
		if err != nil {
			return disabled(EnvTokenExpiresAt + " is neither a YYYY-MM-DD date nor never")
		}
		cfg.expiresAt = expiresAt
	}
	if raw := strings.TrimSpace(getenv(EnvSyncAccount)); raw != "" {
		account, ok := parseSyncAccount(raw)
		if !ok {
			return disabled(EnvSyncAccount + " is neither member:<uuid> nor agent:<uuid>")
		}
		cfg.syncAccount = account
	}
	if raw := strings.TrimSpace(getenv(EnvSyncMinRemaining)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return disabled(EnvSyncMinRemaining + " is not a whole number of requests")
		}
		cfg.syncMinRemaining = n
	}
	return cfg
}

func disabled(reason string) Config {
	return Config{disabledReason: reason}
}

// Enabled reports whether GitHub-native issues are on.
func (c Config) Enabled() bool { return c.enabled }

// PollEnabled reports whether the scheduled poll runs (EnvPollEnabled).
func (c Config) PollEnabled() bool { return c.enabled && c.pollEnabled }

// RefreshOnView reports whether opening a GitHub issue refreshes it
// (EnvRefreshOnView).
func (c Config) RefreshOnView() bool { return c.enabled && c.refreshOnView }

// DisabledReason explains why the feature is off; empty when it is on.
func (c Config) DisabledReason() string { return c.disabledReason }

// TokenExpiresAt returns the registered expiry date (midnight UTC). ok is
// false when the token never expires or the feature is off.
func (c Config) TokenExpiresAt() (expiresAt time.Time, ok bool) {
	return c.expiresAt, c.enabled && !c.expiresAt.IsZero()
}

// LogStatus writes one startup line describing whether the feature is on.
func (c Config) LogStatus(logger *slog.Logger) {
	if c.enabled {
		logger.Info("github issues enabled", "config", c)
		return
	}
	logger.Info("github issues disabled (" + c.disabledReason + ")")
}

func (c Config) expiryString() string {
	if c.expiresAt.IsZero() {
		return tokenNeverExpires
	}
	return c.expiresAt.Format(expiryDateLayout)
}

// String renders the configuration without the token.
func (c Config) String() string {
	if !c.enabled {
		return "githubissues.Config{enabled: false, reason: " + c.disabledReason + "}"
	}
	return "githubissues.Config{enabled: true, token: " + redacted + ", token_expires_at: " + c.expiryString() +
		", poll: " + strconv.FormatBool(c.pollEnabled) + ", refresh_on_view: " + strconv.FormatBool(c.refreshOnView) +
		", sync_min_remaining: " + strconv.Itoa(c.syncMinRemaining) + "}"
}

// GoString keeps %#v from dumping the unexported fields, which fmt would
// otherwise print verbatim.
func (c Config) GoString() string { return c.String() }

// Format routes every fmt verb (%v, %+v, %#v, %s, %q, ...) through String.
func (c Config) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		fmt.Fprintf(f, "%q", c.String())
		return
	}
	fmt.Fprint(f, c.String())
}

// LogValue renders the configuration for slog without the token.
func (c Config) LogValue() slog.Value {
	if !c.enabled {
		return slog.GroupValue(
			slog.Bool("enabled", false),
			slog.String("reason", c.disabledReason),
		)
	}
	return slog.GroupValue(
		slog.Bool("enabled", true),
		slog.String("token", redacted),
		slog.String("token_expires_at", c.expiryString()),
		slog.Bool("poll", c.pollEnabled),
		slog.Bool("refresh_on_view", c.refreshOnView),
		slog.Int("sync_min_remaining", c.syncMinRemaining),
	)
}

// MarshalJSON renders the configuration without the token.
func (c Config) MarshalJSON() ([]byte, error) {
	out := struct {
		Enabled        bool   `json:"enabled"`
		DisabledReason string `json:"disabled_reason,omitempty"`
		TokenExpiresAt string `json:"token_expires_at,omitempty"`
	}{Enabled: c.enabled, DisabledReason: c.disabledReason}
	if c.enabled {
		out.TokenExpiresAt = c.expiryString()
	}
	return json.Marshal(out)
}
