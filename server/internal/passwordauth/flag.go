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
