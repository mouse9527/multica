package passwordauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Every login answered ErrInvalidCredentials runs exactly one argon2id
// computation, whatever made it fail, and every ErrThrottled answer runs
// none. This is the exact form of the timing promise; TestLoginTimingRough
// measures it.
func TestLoginHashesOncePerInvalidAnswer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	ada := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	f.User(t, "Bob", "bob@example.test")
	s, _, _ := testService(t, pool)
	if err := s.SetPassword(ctx, ada, "", testPassword); err != nil {
		t.Fatal(err)
	}
	h := s.store.hasher
	addr := 0
	attempt := func(email, password string) (int64, error) {
		addr++
		before := h.computations.Load()
		_, err := s.Login(ctx, email, password, fmt.Sprintf("203.0.113.%d", addr))
		return h.computations.Load() - before, err
	}
	overlong := strings.Repeat("x", 4*MaxPasswordBytes)

	for _, c := range []struct {
		name, email, password string
		want                  error
	}{
		{"right password", "ada@example.test", testPassword, nil},
		{"wrong password", "ada@example.test", wrongPassword, ErrInvalidCredentials},
		{"overlong password", "ada@example.test", overlong, ErrInvalidCredentials},
		{"no password set", "bob@example.test", wrongPassword, ErrInvalidCredentials},
		{"unknown email", "nobody@example.test", wrongPassword, ErrInvalidCredentials},
		{"unknown email, overlong password", "nobody@example.test", overlong, ErrInvalidCredentials},
	} {
		if n, err := attempt(c.email, c.password); !errors.Is(err, c.want) || n != 1 {
			t.Errorf("%s: %v after %d computations, want %v after 1", c.name, err, n, c.want)
		}
	}

	// Lock all three emails: the real account in the database, the other two
	// in memory. Then even the right password is refused without hashing.
	emails := []string{"ada@example.test", "bob@example.test", "nobody@example.test"}
	for i := 0; i < LockThreshold; i++ {
		for _, email := range emails {
			if _, err := attempt(email, wrongPassword); !errors.Is(err, ErrInvalidCredentials) && !errors.Is(err, ErrThrottled) {
				t.Fatalf("%s: %v", email, err)
			}
		}
	}
	for _, email := range emails {
		if n, err := attempt(email, testPassword); !errors.Is(err, ErrThrottled) || n != 0 {
			t.Errorf("locked %s: %v after %d computations, want %v after 0", email, err, n, ErrThrottled)
		}
	}

	// Over the per-IP limit nothing is hashed either.
	for i := 0; i < IPLimit; i++ {
		_, _ = s.Login(ctx, fmt.Sprintf("spray%d@example.test", i), wrongPassword, "198.51.100.77")
	}
	before := h.computations.Load()
	if _, err := s.Login(ctx, "carol@example.test", wrongPassword, "198.51.100.77"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("over the IP limit: %v", err)
	}
	if n := h.computations.Load() - before; n != 0 {
		t.Errorf("over the IP limit: %d computations", n)
	}
}

// timingSlack is how far apart, as a factor, the median times of answers
// that hash may be, and how much faster an answer that skips hashing must be.
const timingSlack = 3

// TestLoginTimingRough measures what TestLoginHashesOncePerInvalidAnswer
// counts: the median times of a wrong password, of an account without a
// password and of an unknown email stay within timingSlack of each other,
// while a locked account, which skips hashing, is clearly faster, so the
// measurement would notice a missing hash. Hashing is made slow enough to
// dominate the database round trips, and the cases are interleaved so that
// load on the machine affects them alike. It is a coarse check, not a
// statistical side-channel test.
func TestLoginTimingRough(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	ada := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	f.User(t, "Bob", "bob@example.test")
	carol := util.MustParseUUID(f.User(t, "Carol", "carol@example.test"))
	logger := slog.New(slog.DiscardHandler)
	params := Params{MemoryKiB: 16384, Iterations: 3, Parallelism: 1, SaltLen: 16, KeyLen: 32}
	s := NewService(NewStore(pool, newTestHasher(t, params), logger), logger)
	for _, user := range []pgtype.UUID{ada, carol} {
		if err := s.SetPassword(ctx, user, "", testPassword); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.Lock(ctx, carol, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, email string
		want        error
	}{
		{"wrong password", "ada@example.test", ErrInvalidCredentials},
		{"no password set", "bob@example.test", ErrInvalidCredentials},
		{"unknown email", "nobody@example.test", ErrInvalidCredentials},
		{"locked account", "carol@example.test", ErrThrottled},
	}
	const rounds = 15
	samples := make([][]time.Duration, len(cases))
	addr := 0
	for r := 0; r < rounds; r++ {
		for i, c := range cases {
			addr++
			start := time.Now()
			_, err := s.Login(ctx, c.email, wrongPassword, fmt.Sprintf("198.18.%d.%d", addr/250, addr%250+1))
			samples[i] = append(samples[i], time.Since(start))
			if !errors.Is(err, c.want) {
				t.Fatalf("%s: %v, want %v", c.name, err, c.want)
			}
		}
		// Keep the next round clear of the lockout.
		if err := s.store.ResetFailures(ctx, ada); err != nil {
			t.Fatal(err)
		}
		s.phantoms = newPhantomLocks()
	}

	median := func(d []time.Duration) time.Duration {
		d = slices.Clone(d)
		slices.Sort(d)
		return d[len(d)/2]
	}
	medians := make([]time.Duration, len(cases))
	for i, c := range cases {
		medians[i] = median(samples[i])
		t.Logf("%s: median %v over %d rounds", c.name, medians[i], rounds)
	}
	base := medians[0]
	for i := 1; i < 3; i++ {
		if m := medians[i]; m*timingSlack < base || m > base*timingSlack {
			t.Errorf("%s median %v, wrong password %v: more than %dx apart", cases[i].name, m, base, timingSlack)
		}
	}
	if locked := medians[3]; locked*timingSlack > base {
		t.Errorf("locked account median %v is not clearly below a hashed answer (%v): the measurement cannot tell a skipped hash", locked, base)
	}
}
