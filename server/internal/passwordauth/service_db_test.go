package passwordauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// testService returns a Service with cheap hashing, a settable clock and a
// log buffer.
func testService(t *testing.T, pool *pgxpool.Pool) (*Service, *time.Time, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := NewService(NewStore(pool, newTestHasher(t, cheapParams), logger), logger)
	now := time.Now()
	s.now = func() time.Time { return now }
	return s, &now, logs
}

// assertNoSecrets fails if text contains any of the passwords or any stored
// hash.
func assertNoSecrets(t *testing.T, pool *pgxpool.Pool, text string, passwords ...string) {
	t.Helper()
	for _, p := range passwords {
		if strings.Contains(text, p) {
			t.Fatalf("output contains a password: %s", text)
		}
	}
	rows, err := pool.Query(context.Background(), `SELECT password_hash FROM user_credential`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, hash) || strings.Contains(text, hash[strings.LastIndex(hash, "$")+1:]) {
			t.Fatalf("output contains a stored hash")
		}
	}
}

const wrongPassword = "wrong password, twelve+"

func TestLoginLockoutAndRecovery(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	s, now, logs := testService(t, pool)
	if err := s.SetPassword(ctx, user, "", testPassword); err != nil {
		t.Fatal(err)
	}

	got, err := s.Login(ctx, " ADA@example.test ", testPassword, "203.0.113.1")
	if err != nil || got.ID != user {
		t.Fatalf("login: %v", err)
	}

	// Brute force: LockThreshold wrong guesses, then even the right password
	// is refused until the lock expires.
	for i := 0; i < LockThreshold; i++ {
		if _, err := s.Login(ctx, "ada@example.test", wrongPassword, "203.0.113.2"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("guess %d: %v", i+1, err)
		}
	}
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.3"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("locked account accepted a login: %v", err)
	}
	cred, err := s.store.Get(ctx, user)
	if err != nil || cred.FailedCount != LockThreshold || !cred.Locked(*now) {
		t.Fatalf("after brute force: %v %v", cred, err)
	}

	// After the lock one more wrong guess locks for twice as long.
	*now = now.Add(LockBase)
	if _, err := s.Login(ctx, "ada@example.test", wrongPassword, "203.0.113.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("guess after lock: %v", err)
	}
	*now = now.Add(LockBase)
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.4"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("second lock not doubled: %v", err)
	}

	// Once it expires the right password works and clears the count.
	*now = now.Add(LockBase)
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.5"); err != nil {
		t.Fatalf("login after lock expired: %v", err)
	}
	cred, err = s.store.Get(ctx, user)
	if err != nil || cred.FailedCount != 0 || cred.LockedUntil.Valid {
		t.Fatalf("after successful login: %v %v", cred, err)
	}

	if !strings.Contains(logs.String(), "account locked") {
		t.Fatalf("lock not logged: %s", logs)
	}
	assertNoSecrets(t, pool, logs.String(), testPassword, wrongPassword)
}

// An unknown email, a user without a password and a wrong password on a real
// account go through the same sequence of answers, lockout included.
func TestLoginAnswersDoNotRevealAccounts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	withPassword := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	f.User(t, "Bob", "bob@example.test")
	s, now, logs := testService(t, pool)
	if err := s.SetPassword(ctx, withPassword, "", testPassword); err != nil {
		t.Fatal(err)
	}

	trace := func(email string) []error {
		var errs []error
		for i := 0; i < LockThreshold+2; i++ {
			_, err := s.Login(ctx, email, wrongPassword, "198.51.100."+string(rune('1'+i)))
			errs = append(errs, err)
		}
		*now = now.Add(LockBase)
		_, err := s.Login(ctx, email, wrongPassword, "198.51.100.200")
		errs = append(errs, err)
		_, err = s.Login(ctx, email, wrongPassword, "198.51.100.201")
		return append(errs, err)
	}
	start := *now
	want := trace("ada@example.test")
	for _, email := range []string{"bob@example.test", "nobody@example.test"} {
		*now = start
		got := trace(email)
		for i := range want {
			if !errors.Is(got[i], want[i]) {
				t.Fatalf("%s attempt %d: %v, real account got %v", email, i+1, got[i], want[i])
			}
		}
	}
	if !errors.Is(want[0], ErrInvalidCredentials) || !errors.Is(want[LockThreshold], ErrThrottled) {
		t.Fatalf("unexpected trace %v", want)
	}
	if strings.Contains(logs.String(), "nobody@example.test") || strings.Contains(logs.String(), "bob@example.test") {
		t.Fatalf("attempted email logged: %s", logs)
	}
	assertNoSecrets(t, pool, logs.String(), testPassword, wrongPassword)
}

func TestLoginIPLimit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	s, _, _ := testService(t, pool)
	if err := s.SetPassword(ctx, user, "", testPassword); err != nil {
		t.Fatal(err)
	}
	// Spread over many emails so no account lock is involved.
	for i := 0; i < IPLimit; i++ {
		email := "spray" + string(rune('a'+i)) + "@example.test"
		if _, err := s.Login(ctx, email, wrongPassword, "203.0.113.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.9"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("over the IP limit: %v", err)
	}
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.10"); err != nil {
		t.Fatalf("other address: %v", err)
	}
}

// With the per-IP limit off one address can try any number of emails, and
// the account lockout still applies to real accounts and to emails without a
// password alike.
func TestLoginWithoutIPLimit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	s, _, logs := testService(t, pool)
	s.DisableIPLimit()
	if err := s.SetPassword(ctx, user, "", testPassword); err != nil {
		t.Fatal(err)
	}
	const ip = "203.0.113.9"
	for i := 0; i < 2*IPLimit; i++ {
		email := fmt.Sprintf("spray%d@example.test", i)
		if _, err := s.Login(ctx, email, wrongPassword, ip); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := s.Login(ctx, "ada@example.test", testPassword, ip); err != nil {
		t.Fatalf("login after %d attempts from one address: %v", 2*IPLimit, err)
	}

	for _, email := range []string{"ada@example.test", "nobody@example.test"} {
		for i := 0; i < LockThreshold; i++ {
			if _, err := s.Login(ctx, email, wrongPassword, ip); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("%s guess %d: %v", email, i+1, err)
			}
		}
		if _, err := s.Login(ctx, email, testPassword, ip); !errors.Is(err, ErrThrottled) {
			t.Fatalf("%s not locked with the per-IP limit off: %v", email, err)
		}
	}
	if strings.Contains(logs.String(), "rate limited") {
		t.Fatalf("per-IP limit applied: %s", logs)
	}
}

func TestSetPasswordRules(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	s, now, logs := testService(t, pool)
	const second = "second password, long"

	if has, err := s.HasPassword(ctx, user); has || err != nil {
		t.Fatalf("HasPassword before: %v %v", has, err)
	}
	if err := s.SetPassword(ctx, user, "", "short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short first password: %v", err)
	}
	// The first password needs no current one.
	if err := s.SetPassword(ctx, user, "", testPassword); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasPassword(ctx, user); !has || err != nil {
		t.Fatalf("HasPassword after: %v %v", has, err)
	}
	if err := s.SetPassword(ctx, user, "", second); !errors.Is(err, ErrCurrentPasswordRequired) {
		t.Fatalf("change without current: %v", err)
	}
	// Wrong current passwords count towards the lockout.
	for i := 0; i < LockThreshold; i++ {
		if err := s.SetPassword(ctx, user, wrongPassword, second); !errors.Is(err, ErrWrongCurrentPassword) {
			t.Fatalf("wrong current %d: %v", i+1, err)
		}
	}
	if err := s.SetPassword(ctx, user, testPassword, second); !errors.Is(err, ErrThrottled) {
		t.Fatalf("change while locked: %v", err)
	}
	*now = now.Add(LockBase)
	if err := s.SetPassword(ctx, user, testPassword, second); err != nil {
		t.Fatalf("change: %v", err)
	}
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := s.Login(ctx, "ada@example.test", second, "203.0.113.1"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	assertNoSecrets(t, pool, logs.String(), testPassword, wrongPassword, second)
}

// A stored hash larger than the hashing budget (the budget was lowered below
// old parameters) is refused like a wrong password, without counting a
// failure, and the operator is told.
func TestLoginRefusesHashOverBudget(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	old := cheapParams
	old.MemoryKiB = 128
	if err := NewStore(pool, newTestHasher(t, old), nil).SetPassword(ctx, user, testPassword); err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	small, err := NewHasherWithBudget(cheapParams, 64)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(NewStore(pool, small, logger), logger)
	if _, err := s.Login(ctx, "ada@example.test", testPassword, "203.0.113.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login with over-budget hash: %v", err)
	}
	cred, err := s.store.Get(ctx, user)
	if err != nil || cred.FailedCount != 0 {
		t.Fatalf("failure counted: %v %v", cred, err)
	}
	if !strings.Contains(logs.String(), EnvMemoryBudgetKiB) {
		t.Fatalf("operator not told: %s", logs)
	}
	assertNoSecrets(t, pool, logs.String(), testPassword)
}

// waitFor polls cond until it holds or a few seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// oneAtATime returns a Service whose hashing budget fits exactly one
// computation, so the test can fill it and line requests up behind it.
func oneAtATime(t *testing.T, pool *pgxpool.Pool) (*Service, *Hasher) {
	t.Helper()
	h, err := NewHasherWithBudget(cheapParams, cheapParams.MemoryKiB)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	return NewService(NewStore(pool, h, logger), logger), h
}

// Guesses queued for the hashing budget see a lock taken while they waited:
// with the budget full, many concurrent wrong guesses pass the first lock
// check, but once it frees up only LockThreshold of them are computed and
// the rest are refused without hashing. Unknown emails behave the same.
func TestQueuedGuessesStopAtTheLock(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	if err := NewStore(pool, newTestHasher(t, cheapParams), nil).SetPassword(ctx, user, testPassword); err != nil {
		t.Fatal(err)
	}

	for _, email := range []string{"ada@example.test", "nobody@example.test"} {
		t.Run(email, func(t *testing.T) {
			s, h := oneAtATime(t, pool)
			hold, err := h.reserve(ctx, h.budgetKiB)
			if err != nil {
				t.Fatal(err)
			}
			before := h.computations.Load()
			const guesses = 3 * LockThreshold
			results := make(chan error, guesses)
			for i := 0; i < guesses; i++ {
				go func(i int) {
					_, err := s.Login(ctx, email, wrongPassword, fmt.Sprintf("203.0.113.%d", i+1))
					results <- err
				}(i)
			}
			waitFor(t, "all guesses to queue", func() bool { return h.waiting.Load() == guesses })
			hold()

			var invalid, throttled int
			for i := 0; i < guesses; i++ {
				switch err := <-results; {
				case errors.Is(err, ErrInvalidCredentials):
					invalid++
				case errors.Is(err, ErrThrottled):
					throttled++
				default:
					t.Fatalf("guess: %v", err)
				}
			}
			if invalid != LockThreshold || throttled != guesses-LockThreshold {
				t.Fatalf("invalid=%d throttled=%d, want %d and %d", invalid, throttled, LockThreshold, guesses-LockThreshold)
			}
			if n := h.computations.Load() - before; n != LockThreshold {
				t.Fatalf("%d guesses hashed after the budget freed, want %d", n, LockThreshold)
			}
		})
	}
}

// A right password queued for the budget does not sign in once the account
// was locked while it waited, and is not even hashed.
func TestQueuedLoginRefusedAfterLock(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	s, h := oneAtATime(t, pool)
	if err := s.SetPassword(ctx, user, "", testPassword); err != nil {
		t.Fatal(err)
	}
	hold, err := h.reserve(ctx, h.budgetKiB)
	if err != nil {
		t.Fatal(err)
	}
	before := h.computations.Load()
	const logins = 4
	results := make(chan error, logins)
	for i := 0; i < logins; i++ {
		go func(i int) {
			_, err := s.Login(ctx, "ada@example.test", testPassword, fmt.Sprintf("198.51.100.%d", i+1))
			results <- err
		}(i)
	}
	waitFor(t, "logins to queue", func() bool { return h.waiting.Load() == logins })
	if err := s.store.Lock(ctx, user, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	hold()
	for i := 0; i < logins; i++ {
		if err := <-results; !errors.Is(err, ErrThrottled) {
			t.Fatalf("queued login after lock: %v", err)
		}
	}
	if n := h.computations.Load() - before; n != 0 {
		t.Fatalf("%d queued logins hashed after the lock", n)
	}
}

// A client that goes away while its request waits for the budget leaves
// nothing behind: no hashing, no failure counted.
func TestQueuedLoginCancelled(t *testing.T) {
	pool := testPool(t)
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	s, h := oneAtATime(t, pool)
	if err := s.SetPassword(context.Background(), user, "", testPassword); err != nil {
		t.Fatal(err)
	}
	hold, err := h.reserve(context.Background(), h.budgetKiB)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	before := h.computations.Load()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := s.Login(ctx, "ada@example.test", wrongPassword, "192.0.2.10")
		result <- err
	}()
	waitFor(t, "the login to queue", func() bool { return h.waiting.Load() == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login: %v", err)
	}
	if n := h.computations.Load() - before; n != 0 {
		t.Fatalf("cancelled login hashed %d times", n)
	}
	cred, err := s.store.Get(context.Background(), user)
	if err != nil || cred.FailedCount != 0 {
		t.Fatalf("cancelled login counted: %v %v", cred, err)
	}
}
