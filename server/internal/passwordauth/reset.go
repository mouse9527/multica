package passwordauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// ErrUserNotFound means no user has the email. An admin reset never creates
// a user.
var ErrUserNotFound = errors.New("passwordauth: no user with this email")

// UserIDByEmail returns the id of the existing user with email, normalised
// the way the login handlers normalise it, or ErrUserNotFound.
func (s *Store) UserIDByEmail(ctx context.Context, email string) (pgtype.UUID, error) {
	user, err := s.q.GetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrUserNotFound
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("passwordauth: look up user: %w", err)
	}
	return user.ID, nil
}

// AuditMessage is the message of the record AdminReset writes.
const AuditMessage = "passwordauth: audit: password reset by admin command"

// AdminReset sets the password of an existing user from the server-side reset
// command, clearing the failure count and any lock, then writes one JSON audit
// record naming the account and the time to audit. The record goes straight to
// its own handler, so LOG_LEVEL and the Store's logger never filter it out. It
// never holds the password or the hash.
func (s *Store) AdminReset(ctx context.Context, userID pgtype.UUID, email, password string, audit io.Writer) error {
	if err := s.SetPassword(ctx, userID, password); err != nil {
		return err
	}
	now := time.Now()
	record := slog.NewRecord(now, slog.LevelInfo, AuditMessage, 0)
	record.AddAttrs(
		slog.String("user_id", util.UUIDToString(userID)),
		slog.String("email", strings.ToLower(strings.TrimSpace(email))),
		slog.String("at", now.UTC().Format(time.RFC3339)),
	)
	if err := slog.NewJSONHandler(audit, nil).Handle(ctx, record); err != nil {
		return fmt.Errorf("passwordauth: password was set, but writing the audit record failed: %w", err)
	}
	return nil
}
