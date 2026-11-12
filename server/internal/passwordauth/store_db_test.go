package passwordauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	return testutil.IsolatedDB(t, dsn)
}

func TestStoreAgainstDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	other := util.MustParseUUID(f.User(t, "Bob", "bob@example.test"))
	h := newTestHasher(t, cheapParams)
	s := NewStore(pool, h, slog.New(slog.DiscardHandler))

	// No password yet.
	if _, err := s.Get(ctx, user); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Get before SetPassword: %v", err)
	}
	if ok, err := s.CheckPassword(ctx, user, testPassword); ok || !errors.Is(err, ErrNoCredential) {
		t.Fatalf("CheckPassword before SetPassword: ok=%v err=%v", ok, err)
	}
	if _, err := s.RecordFailure(ctx, user); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("RecordFailure before SetPassword: %v", err)
	}

	// A short password is refused and nothing is stored.
	if err := s.SetPassword(ctx, user, "too short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password: %v", err)
	}
	if _, err := s.Get(ctx, user); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("short password was stored: %v", err)
	}

	if err := s.SetPassword(ctx, user, testPassword); err != nil {
		t.Fatal(err)
	}
	cred, err := s.Get(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Algo != Algo || cred.FailedCount != 0 || cred.LockedUntil.Valid || h.NeedsRehash(cred.hash) {
		t.Fatalf("stored credential: %v", cred)
	}
	var stored Params
	if err := pool.QueryRow(ctx, `SELECT params FROM user_credential WHERE user_id = $1`, user).Scan(&stored); err != nil || stored != cheapParams {
		t.Fatalf("params column = %+v, %v; want %+v", stored, err, cheapParams)
	}

	if ok, err := s.CheckPassword(ctx, user, testPassword); !ok || err != nil {
		t.Fatalf("correct password: ok=%v err=%v", ok, err)
	}
	if ok, err := s.CheckPassword(ctx, user, "wrong password!"); ok || err != nil {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
	// Credentials are per user.
	if ok, err := s.CheckPassword(ctx, other, testPassword); ok || !errors.Is(err, ErrNoCredential) {
		t.Fatalf("other user: ok=%v err=%v", ok, err)
	}

	// Failure count and lock.
	for want := int32(1); want <= 3; want++ {
		n, err := s.RecordFailure(ctx, user)
		if err != nil || n != want {
			t.Fatalf("RecordFailure = %d, %v; want %d", n, err, want)
		}
	}
	until := time.Now().Add(15 * time.Minute)
	if err := s.Lock(ctx, user, until); err != nil {
		t.Fatal(err)
	}
	cred, err = s.Get(ctx, user)
	if err != nil || cred.FailedCount != 3 || !cred.Locked(time.Now()) || cred.Locked(until.Add(time.Second)) {
		t.Fatalf("after failures and lock: %v, %v", cred, err)
	}
	if err := s.ResetFailures(ctx, user); err != nil {
		t.Fatal(err)
	}
	if cred, err = s.Get(ctx, user); err != nil || cred.FailedCount != 0 || cred.LockedUntil.Valid {
		t.Fatalf("after reset: %v, %v", cred, err)
	}

	// Setting a new password replaces the old one and clears failures and
	// the lock.
	for range 2 {
		if _, err := s.RecordFailure(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Lock(ctx, user, until); err != nil {
		t.Fatal(err)
	}
	const newPassword = "a different passphrase"
	if err := s.SetPassword(ctx, user, newPassword); err != nil {
		t.Fatal(err)
	}
	if cred, err = s.Get(ctx, user); err != nil || cred.FailedCount != 0 || cred.LockedUntil.Valid {
		t.Fatalf("after new password: %v, %v", cred, err)
	}
	if ok, _ := s.CheckPassword(ctx, user, testPassword); ok {
		t.Fatal("old password still accepted")
	}
	if ok, err := s.CheckPassword(ctx, user, newPassword); !ok || err != nil {
		t.Fatalf("new password: ok=%v err=%v", ok, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_credential WHERE user_id = $1`, user).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows for user = %d, %v; want 1", rows, err)
	}
}

func TestCheckPasswordRehashesAfterParameterUpgrade(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))

	if err := NewStore(pool, newTestHasher(t, cheapParams), nil).SetPassword(ctx, user, testPassword); err != nil {
		t.Fatal(err)
	}
	upgraded := cheapParams
	upgraded.MemoryKiB, upgraded.Iterations = 128, 2
	current := newTestHasher(t, upgraded)
	s := NewStore(pool, current, slog.New(slog.DiscardHandler))
	before, err := s.Get(ctx, user)
	if err != nil || !current.NeedsRehash(before.hash) {
		t.Fatalf("precondition: %v %v", before, err)
	}

	// A wrong password does not rehash.
	if ok, err := s.CheckPassword(ctx, user, "wrong password!"); ok || err != nil {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
	if after, _ := s.Get(ctx, user); after.hash != before.hash {
		t.Fatal("wrong password changed the hash")
	}

	if ok, err := s.CheckPassword(ctx, user, testPassword); !ok || err != nil {
		t.Fatalf("correct password: ok=%v err=%v", ok, err)
	}
	after, err := s.Get(ctx, user)
	if err != nil || after.hash == before.hash || current.NeedsRehash(after.hash) {
		t.Fatalf("hash was not upgraded: %v", err)
	}
	var params json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT params FROM user_credential WHERE user_id = $1`, user).Scan(&params); err != nil {
		t.Fatal(err)
	}
	var stored Params
	if err := json.Unmarshal(params, &stored); err != nil || stored != upgraded {
		t.Fatalf("params column = %s, want %+v", params, upgraded)
	}
	if ok, err := s.CheckPassword(ctx, user, testPassword); !ok || err != nil {
		t.Fatalf("password after rehash: ok=%v err=%v", ok, err)
	}
}

// A rehash computed from a credential read before the password changed must
// not put the old password back.
func TestRehashDoesNotOverwriteNewerPassword(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))

	if err := NewStore(pool, newTestHasher(t, cheapParams), nil).SetPassword(ctx, user, testPassword); err != nil {
		t.Fatal(err)
	}
	upgraded := cheapParams
	upgraded.Iterations = 2
	s := NewStore(pool, newTestHasher(t, upgraded), slog.New(slog.DiscardHandler))
	stale, err := s.Get(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	const newPassword = "a different passphrase"
	if err := s.SetPassword(ctx, user, newPassword); err != nil {
		t.Fatal(err)
	}

	s.rehash(ctx, stale, testPassword)

	if ok, _ := s.CheckPassword(ctx, user, testPassword); ok {
		t.Fatal("stale rehash restored the old password")
	}
	if ok, err := s.CheckPassword(ctx, user, newPassword); !ok || err != nil {
		t.Fatalf("new password: ok=%v err=%v", ok, err)
	}
}
