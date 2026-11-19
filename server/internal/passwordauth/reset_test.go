package passwordauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

func TestAdminResetAgainstDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := testutil.New(pool, "", "")
	user := util.MustParseUUID(f.User(t, "Ada", "ada@example.test"))
	var logs bytes.Buffer
	s := NewStore(pool, newTestHasher(t, cheapParams), slog.New(slog.NewJSONHandler(&logs, nil)))

	// The lookup matches the way login normalises the email.
	got, err := s.UserIDByEmail(ctx, "  Ada@Example.TEST ")
	if err != nil || got != user {
		t.Fatalf("UserIDByEmail = %s, %v; want %s", util.UUIDToString(got), err, util.UUIDToString(user))
	}
	if _, err := s.UserIDByEmail(ctx, "nobody@example.test"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown email: %v", err)
	}

	// A locked account with failed attempts and an old password.
	if err := s.SetPassword(ctx, user, "an old password here"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := s.RecordFailure(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Lock(ctx, user, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	logs.Reset()

	if err := s.AdminReset(ctx, user, " Ada@Example.test", testPassword); err != nil {
		t.Fatal(err)
	}
	cred, err := s.Get(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if cred.FailedCount != 0 || cred.LockedUntil.Valid || cred.Locked(time.Now()) {
		t.Fatalf("after reset: %v", cred)
	}
	if ok, err := s.CheckPassword(ctx, user, testPassword); !ok || err != nil {
		t.Fatalf("new password: ok=%v err=%v", ok, err)
	}
	if ok, err := s.CheckPassword(ctx, user, "an old password here"); ok || err != nil {
		t.Fatalf("old password: ok=%v err=%v", ok, err)
	}

	// One audit record naming the account and the time, without secrets.
	record := logs.String()
	for _, want := range []string{"audit", util.UUIDToString(user), `"email":"ada@example.test"`, `"at":"`} {
		if !strings.Contains(record, want) {
			t.Errorf("audit record lacks %q: %s", want, record)
		}
	}
	if n := strings.Count(record, "\n"); n != 1 {
		t.Errorf("want 1 log record, got %d", n)
	}
	if strings.Contains(record, testPassword) || strings.Contains(record, cred.hash) || strings.Contains(record, "$argon2id$") {
		t.Fatal("audit record contains the password or the hash")
	}

	// A short password is refused: the old one stays and nothing is logged.
	logs.Reset()
	if err := s.AdminReset(ctx, user, "ada@example.test", "too short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password: %v", err)
	}
	if ok, err := s.CheckPassword(ctx, user, testPassword); !ok || err != nil {
		t.Fatalf("password after refused reset: ok=%v err=%v", ok, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("refused reset was logged: %s", logs.String())
	}
}
