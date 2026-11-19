package passwordauth

import (
	"context"
	"errors"
	"fmt"
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

// AdminReset sets the password of an existing user from the server-side reset
// command, clearing the failure count and any lock, and writes one audit
// record naming the account and the time. The record never holds the
// password or the hash.
func (s *Store) AdminReset(ctx context.Context, userID pgtype.UUID, email, password string) error {
	if err := s.SetPassword(ctx, userID, password); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "passwordauth: audit: password reset by admin command",
		"user_id", util.UUIDToString(userID),
		"email", strings.ToLower(strings.TrimSpace(email)),
		"at", time.Now().UTC().Format(time.RFC3339),
	)
	return nil
}
