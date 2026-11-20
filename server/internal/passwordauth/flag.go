package passwordauth

import "strings"

// EnvEnabled is the deployment switch for password login. Off by default:
// while it is off no password route is registered and /api/config does not
// mention passwords, so the server behaves as upstream.
const EnvEnabled = "MULTICA_PASSWORD_AUTH_ENABLED"

// Enabled reports whether password login is switched on. getenv is os.Getenv
// in production; "true" and "1" (any case, surrounding spaces ignored) turn
// it on, anything else leaves it off.
func Enabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(EnvEnabled))) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// EnvIPLimit switches the per-IP limit on login attempts. On by default; "0"
// or "false" (any case, surrounding spaces ignored) turns it off, which leaves
// the account lockout as the only throttle. It is a switch, not the limit
// itself: any other value keeps IPLimit.
const EnvIPLimit = "MULTICA_PASSWORD_IP_LIMIT"

// IPLimitEnabled reports whether the per-IP limit is on. getenv is os.Getenv
// in production.
func IPLimitEnabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(EnvIPLimit))) {
	case "0", "false":
		return false
	default:
		return true
	}
}
