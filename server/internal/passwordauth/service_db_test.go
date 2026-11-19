package passwordauth

import (
	"bytes"
	"context"
	"errors"
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
