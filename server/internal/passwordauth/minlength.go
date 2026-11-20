// Added by the mouse9527/multica fork: configurable minimum password length
// (M-184).

package passwordauth

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// EnvMinLength overrides the minimum length of a new password, in
// characters. Unset keeps DefaultMinPasswordRunes.
const EnvMinLength = "MULTICA_PASSWORD_MIN_LENGTH"

const (
	// DefaultMinPasswordRunes is the minimum password length, counted in
	// characters, when EnvMinLength is not set.
	DefaultMinPasswordRunes = 12
	// MinPasswordRunesFloor is the lowest minimum EnvMinLength can choose;
	// a lower value is raised to it.
	MinPasswordRunesFloor = 6
)

// ErrPasswordTooShort matches, through errors.Is, the error a password below
// the configured minimum gets. That error's text names the minimum.
var ErrPasswordTooShort = errors.New("passwordauth: password is too short")

type tooShortError struct{ min int }

func (e tooShortError) Error() string {
	return fmt.Sprintf("passwordauth: password must be at least %d characters", e.min)
}

func (e tooShortError) Is(target error) bool { return target == ErrPasswordTooShort }

// MinPasswordRunesFromEnv returns the minimum password length EnvMinLength
// chooses. Unset or blank gives DefaultMinPasswordRunes. A value that is not
// an integer is logged and also gives the default, so a typo never weakens
// the rule; a value below MinPasswordRunesFloor is logged and raised to it.
// getenv is os.Getenv in production; a nil logger uses slog.Default().
func MinPasswordRunesFromEnv(getenv func(string) string, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.Default()
	}
	v := strings.TrimSpace(getenv(EnvMinLength))
	if v == "" {
		return DefaultMinPasswordRunes
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logger.Warn("password minimum length: "+EnvMinLength+" is not an integer; using the default",
			"default", DefaultMinPasswordRunes)
		return DefaultMinPasswordRunes
	}
	if n < MinPasswordRunesFloor {
		logger.Warn("password minimum length: "+EnvMinLength+" is below the floor; using the floor",
			"configured", n, "floor", MinPasswordRunesFloor)
		return MinPasswordRunesFloor
	}
	return n
}
