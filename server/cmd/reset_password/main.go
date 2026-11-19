// Command reset_password sets or resets the login password of an existing
// user and clears the user's failed attempts and lock. It is the fallback for
// a forgotten password when the server cannot send email.
//
// This command is fork-only (mouse9527/multica); upstream multica-ai/multica
// has no password login. See docs/password-auth.md.
//
// The password is never a command-line argument, so it stays out of shell
// history and the process list. On a terminal it is prompted for twice with
// echo off; otherwise stdin must hold exactly the password, optionally
// followed by one newline. The command connects to DATABASE_URL, never
// creates a user, and hashes with the same MULTICA_PASSWORD_ARGON2_*
// settings as the server.
//
//	reset_password --email user@example.com
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/passwordauth"
	"github.com/multica-ai/multica/server/internal/util"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

var (
	errPasswordMismatch = errors.New("the two passwords do not match")
	errMultiline        = errors.New("stdin must hold the password on a single line")
)

func main() {
	logger.Init()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr, slog.Default())
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdin *os.File, stdout, stderr io.Writer, log *slog.Logger) int {
	flags := flag.NewFlagSet("reset_password", flag.ContinueOnError)
	flags.SetOutput(stderr)
	email := flags.String("email", "", "email of the existing user whose password is set")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: reset_password --email <email>")
		fmt.Fprintln(stderr, "The new password is read from stdin (prompted for on a terminal), never from arguments.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *email == "" || flags.NArg() != 0 {
		flags.Usage()
		return exitUsage
	}

	fail := func(err error) int {
		fmt.Fprintln(stderr, "reset_password:", err)
		return exitError
	}

	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return fail(errors.New("DATABASE_URL is not set"))
	}
	params, err := passwordauth.ParamsFromEnv(getenv)
	if err != nil {
		return fail(err)
	}
	hasher, err := passwordauth.NewHasher(params)
	if err != nil {
		return fail(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fail(fmt.Errorf("connect to database: %w", err))
	}
	defer pool.Close()
	store := passwordauth.NewStore(pool, hasher, log)

	// Look the user up before asking for a password, so a mistyped email is
	// reported before anyone types a secret.
	userID, err := store.UserIDByEmail(ctx, *email)
	if err != nil {
		return fail(err)
	}
	password, err := readPassword(ctx, stdin, stderr)
	if err != nil {
		return fail(err)
	}
	if err := store.AdminReset(ctx, userID, *email, password); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Password set for user %s; failed attempts and lock cleared.\n", util.UUIDToString(userID))
	return exitOK
}

// readPassword prompts twice with echo off when stdin is a terminal, and
// otherwise reads the whole of stdin as the password.
func readPassword(ctx context.Context, stdin *os.File, prompt io.Writer) (string, error) {
	restore, err := disableEcho(stdin)
	if err != nil {
		return readPiped(stdin)
	}
	defer restore()
	return promptTwice(ctx, stdin, prompt)
}

// promptTwice asks for the password and its confirmation, one line each.
func promptTwice(ctx context.Context, terminal io.Reader, prompt io.Writer) (string, error) {
	lines := bufio.NewReader(io.LimitReader(terminal, 2*(passwordauth.MaxPasswordBytes+2)))
	first, err := promptLine(ctx, lines, prompt, "New password: ")
	if err != nil {
		return "", err
	}
	second, err := promptLine(ctx, lines, prompt, "Repeat password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errPasswordMismatch
	}
	return first, nil
}

// promptLine reads one line from the terminal. The read runs in a goroutine
// so an interrupt returns at once and the deferred restore turns echo back
// on; a blocked read would otherwise ignore the signal.
func promptLine(ctx context.Context, lines *bufio.Reader, prompt io.Writer, label string) (string, error) {
	fmt.Fprint(prompt, label)
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := lines.ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(prompt)
		return "", ctx.Err()
	case r := <-done:
		// Echo is off, so the newline the user typed was not shown.
		fmt.Fprintln(prompt)
		if r.err != nil && !(errors.Is(r.err, io.EOF) && r.line != "") {
			return "", errors.New("no password entered")
		}
		return trimNewline(r.line), nil
	}
}

func readPiped(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, passwordauth.MaxPasswordBytes+3))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	password := trimNewline(string(data))
	if strings.ContainsAny(password, "\r\n") {
		return "", errMultiline
	}
	if len(password) > passwordauth.MaxPasswordBytes {
		return "", passwordauth.ErrPasswordTooLong
	}
	return password, nil
}

func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
		if n := len(s); n > 0 && s[n-1] == '\r' {
			s = s[:n-1]
		}
	}
	return s
}
