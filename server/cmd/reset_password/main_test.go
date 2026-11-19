package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/passwordauth"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

const testPassword = "correct horse battery"

// cheapEnv hashes with the cheapest parameters ParamsFromEnv accepts.
var cheapEnv = map[string]string{
	passwordauth.EnvMemoryKiB:   "19456",
	passwordauth.EnvIterations:  "2",
	passwordauth.EnvParallelism: "1",
}

func stdinFile(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestReadPiped(t *testing.T) {
	long := strings.Repeat("x", passwordauth.MaxPasswordBytes+1)
	for _, tc := range []struct {
		name, in, want string
		err            error
	}{
		{name: "newline", in: testPassword + "\n", want: testPassword},
		{name: "crlf", in: testPassword + "\r\n", want: testPassword},
		{name: "no newline", in: testPassword, want: testPassword},
		{name: "spaces kept", in: " " + testPassword + " \n", want: " " + testPassword + " "},
		{name: "two lines", in: testPassword + "\n" + testPassword + "\n", err: errMultiline},
		{name: "blank line after", in: testPassword + "\n\n", err: errMultiline},
		{name: "too long", in: long + "\n", err: passwordauth.ErrPasswordTooLong},
		{name: "far too long", in: long + long, err: passwordauth.ErrPasswordTooLong},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPiped(strings.NewReader(tc.in))
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("readPiped = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestPromptTwice(t *testing.T) {
	ctx := context.Background()
	var prompt bytes.Buffer
	got, err := promptTwice(ctx, strings.NewReader(testPassword+"\n"+testPassword+"\n"), &prompt)
	if err != nil || got != testPassword {
		t.Fatalf("matching entries: %q, %v", got, err)
	}
	if strings.Contains(prompt.String(), testPassword) {
		t.Fatal("prompt output echoes the password")
	}
	if _, err := promptTwice(ctx, strings.NewReader(testPassword+"\nsomething else entirely\n"), io.Discard); !errors.Is(err, errPasswordMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := promptTwice(ctx, strings.NewReader(testPassword+"\n"), io.Discard); err == nil {
		t.Fatal("missing confirmation accepted")
	}

	// An interrupt returns while the read is still blocked.
	r, w := io.Pipe()
	defer w.Close()
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := promptTwice(cctx, r, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted prompt: %v", err)
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--email", ""},
		{"--email", "ada@example.test", "extra"},
		// There is no way to pass the password as an argument.
		{"--email", "ada@example.test", "--password", testPassword},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, func(string) string { return "" }, stdinFile(t, ""), &stdout, &stderr, slog.New(slog.DiscardHandler))
		if code != exitUsage {
			t.Errorf("run(%q) = %d; want %d", args, code, exitUsage)
		}
		if strings.Contains(stdout.String()+stderr.String(), testPassword) {
			t.Errorf("run(%q) output contains the password", args)
		}
	}
}

func TestRunAgainstDatabase(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	ctx := context.Background()
	user := util.MustParseUUID(testutil.New(pool, "", "").User(t, "Ada", "ada@example.test"))
	env := map[string]string{"DATABASE_URL": pool.Config().ConnConfig.ConnString()}
	for k, v := range cheapEnv {
		env[k] = v
	}
	getenv := func(k string) string { return env[k] }
	params, err := passwordauth.ParamsFromEnv(getenv)
	if err != nil {
		t.Fatal(err)
	}
	hasher, err := passwordauth.NewHasher(params)
	if err != nil {
		t.Fatal(err)
	}
	store := passwordauth.NewStore(pool, hasher, slog.New(slog.DiscardHandler))

	// Locked after failed attempts.
	if err := store.SetPassword(ctx, user, "an old password here"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := store.RecordFailure(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Lock(ctx, user, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	runWith := func(args []string, stdin string) (int, string) {
		t.Helper()
		var stdout, stderr, logs bytes.Buffer
		code := run(ctx, args, getenv, stdinFile(t, stdin), &stdout, &stderr, slog.New(slog.NewJSONHandler(&logs, nil)))
		all := stdout.String() + stderr.String() + logs.String()
		if strings.Contains(all, testPassword) || strings.Contains(all, "$argon2id$") {
			t.Fatalf("output contains the password or a hash:\n%s", all)
		}
		return code, all
	}

	// An unknown email is refused and creates nothing.
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM "user"`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	code, out := runWith([]string{"--email", "nobody@example.test"}, testPassword+"\n")
	if code != exitError || !strings.Contains(out, passwordauth.ErrUserNotFound.Error()) {
		t.Fatalf("unknown email: code %d, output %s", code, out)
	}
	var after, creds int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM "user"), (SELECT count(*) FROM user_credential)`).Scan(&after, &creds); err != nil {
		t.Fatal(err)
	}
	if after != before || creds != 1 {
		t.Fatalf("unknown email changed rows: users %d -> %d, credentials %d", before, after, creds)
	}

	// A too-short password is refused and the lock stays.
	if code, out := runWith([]string{"--email", "ada@example.test"}, "short\n"); code != exitError || !strings.Contains(out, "at least") {
		t.Fatalf("short password: code %d, output %s", code, out)
	}
	if cred, err := store.Get(ctx, user); err != nil || !cred.Locked(time.Now()) {
		t.Fatalf("refused reset changed the credential: %v, %v", cred, err)
	}

	// The reset sets the password, clears the failures and the lock, and logs
	// one audit record.
	code, out = runWith([]string{"--email", "Ada@Example.test"}, testPassword+"\n")
	if code != exitOK || !strings.Contains(out, util.UUIDToString(user)) || !strings.Contains(out, "audit") {
		t.Fatalf("reset: code %d, output %s", code, out)
	}
	cred, err := store.Get(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if cred.FailedCount != 0 || cred.LockedUntil.Valid {
		t.Fatalf("after reset: %v", cred)
	}
	if ok, err := store.CheckPassword(ctx, user, testPassword); !ok || err != nil {
		t.Fatalf("login with the new password: ok=%v err=%v", ok, err)
	}
}
