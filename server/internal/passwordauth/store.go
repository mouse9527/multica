package passwordauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrNoCredential means the user has not set a password.
var ErrNoCredential = errors.New("passwordauth: user has no password")

// Credential is a user_credential row. The hash stays unexported and is left
// out of every printed, logged or JSON form.
type Credential struct {
	UserID      pgtype.UUID
	Algo        string
	FailedCount int32
	// LockedUntil is invalid (NULL) when the account is not locked.
	LockedUntil pgtype.Timestamptz
	CreatedAt   pgtype.Timestamptz
	UpdatedAt   pgtype.Timestamptz
	hash        string
}

// Locked reports whether the account is locked at now.
func (c Credential) Locked(now time.Time) bool {
	return c.LockedUntil.Valid && now.Before(c.LockedUntil.Time)
}

func (c Credential) String() string {
	locked := "none"
	if c.LockedUntil.Valid {
		locked = c.LockedUntil.Time.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("Credential{UserID:%s Algo:%s FailedCount:%d LockedUntil:%s}",
		util.UUIDToString(c.UserID), c.Algo, c.FailedCount, locked)
}

// Format makes every fmt verb, including %#v and %d, print String.
func (c Credential) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(c.String()))
}

// LogValue keeps the hash out of slog records.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user_id", util.UUIDToString(c.UserID)),
		slog.String("algo", c.Algo),
		slog.Int("failed_count", int(c.FailedCount)),
	)
}

// Store reads and writes user_credential.
type Store struct {
	q      *db.Queries
	hasher *Hasher
	logger *slog.Logger
}

// NewStore returns a Store over dbtx that hashes with hasher.
func NewStore(dbtx db.DBTX, hasher *Hasher, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{q: db.New(dbtx), hasher: hasher, logger: logger}
}

// Get returns the user's credential, or ErrNoCredential.
func (s *Store) Get(ctx context.Context, userID pgtype.UUID) (Credential, error) {
	row, err := s.q.GetUserCredential(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, fmt.Errorf("passwordauth: get credential: %w", err)
	}
	return Credential{
		UserID:      row.UserID,
		Algo:        row.Algo,
		FailedCount: row.FailedCount,
		LockedUntil: row.LockedUntil,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		hash:        row.PasswordHash,
	}, nil
}

// SetPassword validates password and stores its hash as the user's password,
// clearing the failure count and any lock. The caller has checked that the
// user exists and may set it.
func (s *Store) SetPassword(ctx context.Context, userID pgtype.UUID, password string) error {
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return err
	}
	params, err := json.Marshal(s.hasher.Params())
	if err != nil {
		return fmt.Errorf("passwordauth: encode params: %w", err)
	}
	if err := s.q.UpsertUserCredential(ctx, db.UpsertUserCredentialParams{
		UserID:       userID,
		PasswordHash: hash,
		Algo:         Algo,
		Params:       params,
	}); err != nil {
		return fmt.Errorf("passwordauth: store credential: %w", err)
	}
	return nil
}

// CheckPassword reports whether password is the user's password. A user
// without one gets ErrNoCredential after the same hashing work as a wrong
// password. On a match with outdated parameters the hash is replaced; failing
// to replace it is logged and does not fail the check. Failure counting and
// locks are the caller's policy.
func (s *Store) CheckPassword(ctx context.Context, userID pgtype.UUID, password string) (bool, error) {
	cred, err := s.Get(ctx, userID)
	if errors.Is(err, ErrNoCredential) {
		s.hasher.VerifyDummy(password)
		return false, err
	}
	if err != nil {
		return false, err
	}
	ok, err := s.hasher.Verify(password, cred.hash)
	if err != nil || !ok {
		return false, err
	}
	if s.hasher.NeedsRehash(cred.hash) {
		s.rehash(ctx, cred, password)
	}
	return true, nil
}

func (s *Store) rehash(ctx context.Context, cred Credential, password string) {
	hash, err := s.hasher.hash(password)
	if err != nil {
		s.logger.WarnContext(ctx, "passwordauth: rehash failed", "user_id", util.UUIDToString(cred.UserID), "error", err)
		return
	}
	params, err := json.Marshal(s.hasher.Params())
	if err != nil {
		s.logger.WarnContext(ctx, "passwordauth: rehash failed", "user_id", util.UUIDToString(cred.UserID), "error", err)
		return
	}
	// Zero rows means the password changed since it was read; the newer
	// password wins.
	if _, err := s.q.RehashUserCredential(ctx, db.RehashUserCredentialParams{
		NewPasswordHash: hash,
		Algo:            Algo,
		Params:          params,
		UserID:          cred.UserID,
		OldPasswordHash: cred.hash,
	}); err != nil {
		s.logger.WarnContext(ctx, "passwordauth: rehash failed", "user_id", util.UUIDToString(cred.UserID), "error", err)
	}
}

// RecordFailure counts one failed attempt and returns the new count. A user
// without a credential gets ErrNoCredential.
func (s *Store) RecordFailure(ctx context.Context, userID pgtype.UUID) (int32, error) {
	n, err := s.q.RecordUserCredentialFailure(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoCredential
	}
	if err != nil {
		return 0, fmt.Errorf("passwordauth: record failure: %w", err)
	}
	return n, nil
}

// Lock locks the account until the given time.
func (s *Store) Lock(ctx context.Context, userID pgtype.UUID, until time.Time) error {
	if err := s.q.LockUserCredential(ctx, db.LockUserCredentialParams{
		UserID:      userID,
		LockedUntil: pgtype.Timestamptz{Time: until, Valid: true},
	}); err != nil {
		return fmt.Errorf("passwordauth: lock: %w", err)
	}
	return nil
}

// ResetFailures clears the failure count and any lock.
func (s *Store) ResetFailures(ctx context.Context, userID pgtype.UUID) error {
	if err := s.q.ResetUserCredentialFailures(ctx, userID); err != nil {
		return fmt.Errorf("passwordauth: reset failures: %w", err)
	}
	return nil
}
