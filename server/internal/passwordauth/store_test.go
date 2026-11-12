package passwordauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// fakeDB answers GetUserCredential with row and fails every Exec with
// execErr. Postgres puts the failing row, hash included, in an error's
// Detail; the fake does the same so the tests prove such details stay out of
// errors and logs.
type fakeDB struct {
	row     *Credential
	params  []byte
	execErr error
}

func (f *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeDB: Query not supported")
}

func (f *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{db: f}
}

type fakeRow struct{ db *fakeDB }

func (r fakeRow) Scan(dest ...any) error {
	c := r.db.row
	if c == nil {
		return pgx.ErrNoRows
	}
	if len(dest) != 8 {
		return fmt.Errorf("fakeRow: %d columns", len(dest))
	}
	*dest[0].(*pgtype.UUID) = c.UserID
	*dest[1].(*string) = c.hash
	*dest[2].(*string) = c.Algo
	*dest[3].(*[]byte) = r.db.params
	*dest[4].(*int32) = c.FailedCount
	*dest[5].(*pgtype.Timestamptz) = c.LockedUntil
	*dest[6].(*pgtype.Timestamptz) = c.CreatedAt
	*dest[7].(*pgtype.Timestamptz) = c.UpdatedAt
	return nil
}

func leakyPgError(secrets ...string) error {
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     "23514",
		Message:  `new row for relation "user_credential" violates check constraint`,
		Detail:   "Failing row contains (" + strings.Join(secrets, ", ") + ").",
	}
}

func assertNoSecret(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Fatalf("%s contains a secret: %q", where, text)
		}
	}
	if strings.Contains(text, "$argon2id$") {
		t.Fatalf("%s contains a hash: %q", where, text)
	}
}

func TestSetPasswordErrorHidesPasswordAndHash(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	fake := &fakeDB{execErr: leakyPgError(testPassword, "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$a2V5")}
	var logs bytes.Buffer
	s := NewStore(fake, h, slog.New(slog.NewTextHandler(&logs, nil)))

	err := s.SetPassword(context.Background(), util.MustParseUUID("00000000-0000-0000-0000-000000000001"), testPassword)
	if err == nil {
		t.Fatal("SetPassword succeeded on a failing database")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatal("database error is not reachable with errors.As")
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
		assertNoSecret(t, "error", text, testPassword)
	}
	assertNoSecret(t, "log", logs.String(), testPassword)
}

func TestRehashFailureLogHidesPasswordAndHash(t *testing.T) {
	old := newTestHasher(t, cheapParams)
	stored, err := old.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := cheapParams
	upgraded.Iterations = 2
	current := newTestHasher(t, upgraded)

	fake := &fakeDB{
		row: &Credential{
			UserID: util.MustParseUUID("00000000-0000-0000-0000-000000000002"),
			Algo:   Algo,
			hash:   stored,
		},
		params:  []byte(`{}`),
		execErr: leakyPgError(testPassword, stored),
	}
	for _, handler := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var logs bytes.Buffer
		s := NewStore(fake, current, slog.New(handler(&logs)))
		ok, err := s.CheckPassword(context.Background(), fake.row.UserID, testPassword)
		if !ok || err != nil {
			t.Fatalf("a failed rehash must not fail the login: ok=%v err=%v", ok, err)
		}
		if !strings.Contains(logs.String(), "rehash failed") {
			t.Fatalf("rehash failure not logged: %q", logs.String())
		}
		assertNoSecret(t, "log", logs.String(), testPassword, stored)
	}
}

func TestCheckPasswordErrorsHidePassword(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	id := util.MustParseUUID("00000000-0000-0000-0000-000000000003")

	// No credential.
	s := NewStore(&fakeDB{}, h, slog.New(slog.DiscardHandler))
	ok, err := s.CheckPassword(context.Background(), id, testPassword)
	if ok || !errors.Is(err, ErrNoCredential) {
		t.Fatalf("no credential: ok=%v err=%v", ok, err)
	}
	assertNoSecret(t, "error", err.Error(), testPassword)

	// A corrupt stored hash.
	corrupt := "$argon2id$v=19$m=64,t=1,p=1$" + testPassword + "$x"
	s = NewStore(&fakeDB{row: &Credential{UserID: id, Algo: Algo, hash: corrupt}}, h, slog.New(slog.DiscardHandler))
	ok, err = s.CheckPassword(context.Background(), id, testPassword)
	if ok || !errors.Is(err, ErrMalformedHash) {
		t.Fatalf("corrupt hash: ok=%v err=%v", ok, err)
	}
	assertNoSecret(t, "error", err.Error(), testPassword, corrupt)
}

func TestCredentialNeverPrintsHash(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	hash, err := h.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	c := Credential{
		UserID:      util.MustParseUUID("00000000-0000-0000-0000-000000000004"),
		Algo:        Algo,
		FailedCount: 2,
		LockedUntil: pgtype.Timestamptz{Time: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
		hash:        hash,
	}

	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
		out = append(out, fmt.Sprintf(verb, c), fmt.Sprintf(verb, &c), fmt.Sprintf(verb, []Credential{c}))
	}
	js, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, string(js))
	var text, jsonLog bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("credential", "cred", c)
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("credential", "cred", c)
	out = append(out, text.String(), jsonLog.String())

	for _, s := range out {
		assertNoSecret(t, "output", s, hash)
	}
	if !strings.Contains(c.String(), "00000000-0000-0000-0000-000000000004") || !strings.Contains(text.String(), "failed_count=2") {
		t.Fatalf("redacted forms lost the non-secret fields: %q / %q", c.String(), text.String())
	}
}

func TestCredentialLocked(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if (Credential{}).Locked(now) {
		t.Fatal("NULL locked_until is locked")
	}
	until := pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true}
	if !(Credential{LockedUntil: until}).Locked(now) {
		t.Fatal("future locked_until is not locked")
	}
	if (Credential{LockedUntil: until}).Locked(now.Add(time.Minute)) {
		t.Fatal("expired locked_until is still locked")
	}
}
