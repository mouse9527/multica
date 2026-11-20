// Added by the mouse9527/multica fork: configurable minimum password length
// (M-184).

package passwordauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestMinPasswordRunesFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int
		warns bool
	}{
		{name: "unset", value: "", want: DefaultMinPasswordRunes},
		{name: "blank", value: "  ", want: DefaultMinPasswordRunes},
		{name: "custom", value: "8", want: 8},
		{name: "custom with spaces", value: " 8 ", want: 8},
		{name: "at the floor", value: "6", want: MinPasswordRunesFloor},
		{name: "above the default", value: "16", want: 16},
		{name: "below the floor", value: "4", want: MinPasswordRunesFloor, warns: true},
		{name: "zero", value: "0", want: MinPasswordRunesFloor, warns: true},
		{name: "negative", value: "-3", want: MinPasswordRunesFloor, warns: true},
		{name: "not an integer", value: "eight", want: DefaultMinPasswordRunes, warns: true},
		{name: "fraction", value: "7.5", want: DefaultMinPasswordRunes, warns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			getenv := func(k string) string {
				if k == EnvMinLength {
					return tc.value
				}
				return ""
			}
			if got := MinPasswordRunesFromEnv(getenv, logger); got != tc.want {
				t.Fatalf("MinPasswordRunesFromEnv(%q) = %d, want %d", tc.value, got, tc.want)
			}
			warned := strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), EnvMinLength)
			if warned != tc.warns {
				t.Fatalf("warned = %v, want %v; logs: %s", warned, tc.warns, logs.String())
			}
		})
	}
}

// hasherWithMinLength builds a Hasher the way the server and reset_password
// do, with the cheapest parameters ParamsFromEnv accepts.
func hasherWithMinLength(t *testing.T, value string) *Hasher {
	t.Helper()
	env := map[string]string{
		EnvMemoryKiB:   "19456",
		EnvIterations:  "2",
		EnvParallelism: "1",
		EnvMinLength:   value,
	}
	h, err := HasherFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHasherFromEnvAppliesMinLength(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", value: "", want: DefaultMinPasswordRunes},
		{name: "custom", value: "8", want: 8},
		{name: "below the floor", value: "3", want: MinPasswordRunesFloor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := hasherWithMinLength(t, tc.value)
			if got := h.MinPasswordRunes(); got != tc.want {
				t.Fatalf("MinPasswordRunes = %d, want %d", got, tc.want)
			}
			// Hash is what setting, changing and resetting a password all
			// go through.
			if _, err := h.Hash(strings.Repeat("密", tc.want)); err != nil {
				t.Fatalf("%d characters: %v", tc.want, err)
			}
			short := strings.Repeat("a", tc.want-1)
			_, err := h.Hash(short)
			if !errors.Is(err, ErrPasswordTooShort) {
				t.Fatalf("%d characters: err = %v, want ErrPasswordTooShort", tc.want-1, err)
			}
			if !strings.Contains(err.Error(), "at least "+strconv.Itoa(tc.want)+" characters") || strings.Contains(err.Error(), short) {
				t.Fatalf("too short error = %q", err)
			}
		})
	}
}

// Service.SetPassword checks the configured minimum before it reads the
// store, so a store without a database is enough here.
func TestServiceSetPasswordUsesConfiguredMinLength(t *testing.T) {
	svc := NewService(NewStore(nil, hasherWithMinLength(t, "8"), nil), nil)
	if got := svc.MinPasswordRunes(); got != 8 {
		t.Fatalf("MinPasswordRunes = %d, want 8", got)
	}
	err := svc.SetPassword(context.Background(), pgtype.UUID{}, "", "seven77")
	if !errors.Is(err, ErrPasswordTooShort) || !strings.Contains(err.Error(), "at least 8 characters") {
		t.Fatalf("7 characters: err = %v", err)
	}
}
