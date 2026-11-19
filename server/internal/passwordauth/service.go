package passwordauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Errors returned by Service. Like the rest of the package they never carry
// a password, a hash or the email that was tried.
var (
	// ErrInvalidCredentials covers an unknown email, an account without a
	// password and a wrong password alike, so the answer does not tell which.
	ErrInvalidCredentials = errors.New("passwordauth: invalid email or password")
	// ErrThrottled covers both the account lockout and the per-IP limit.
	ErrThrottled = errors.New("passwordauth: too many attempts")
	// ErrCurrentPasswordRequired: changing an existing password needs it.
	ErrCurrentPasswordRequired = errors.New("passwordauth: current password is required")
	// ErrWrongCurrentPassword: the current password given to a change is
	// wrong. It counts towards the account lockout like a failed login.
	ErrWrongCurrentPassword = errors.New("passwordauth: current password is incorrect")
)

// Service is the password-login policy: who may log in, account lockout,
// the per-IP limit, and setting a password.
type Service struct {
	store    *Store
	logger   *slog.Logger
	now      func() time.Time
	ips      *ipLimiter
	phantoms *phantomLocks
}

// NewService returns a Service over store. logger may be nil.
func NewService(store *Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:    store,
		logger:   logger,
		now:      time.Now,
		ips:      newIPLimiter(),
		phantoms: newPhantomLocks(),
	}
}

// NormalizeEmail is how emails are compared, as in verification-code login.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Login checks email and password for a request from ip and returns the
// user. Failures are ErrInvalidCredentials or ErrThrottled; any other error
// is an internal one. Only an existing user with a password can succeed.
//
// Every path that answers ErrInvalidCredentials runs one argon2id
// verification, and every path that answers ErrThrottled runs none, so
// timing does not separate the cases either.
func (s *Service) Login(ctx context.Context, email, password, ip string) (db.User, error) {
	if !s.ips.allow(ip, s.now()) {
		s.logger.WarnContext(ctx, "password login: rate limited", "ip", ip)
		return db.User{}, ErrThrottled
	}

	email = NormalizeEmail(email)
	user, err := s.store.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, s.failWithoutPassword(ctx, email, password, ip)
	}
	if err != nil {
		return db.User{}, fmt.Errorf("passwordauth: look up user: %w", err)
	}
	cred, err := s.store.Get(ctx, user.ID)
	if errors.Is(err, ErrNoCredential) {
		return db.User{}, s.failWithoutPassword(ctx, email, password, ip)
	}
	if err != nil {
		return db.User{}, err
	}

	now := s.now()
	if cred.Locked(now) {
		s.logger.WarnContext(ctx, "password login: account locked", "user_id", util.UUIDToString(user.ID), "ip", ip)
		return db.User{}, ErrThrottled
	}
	ok, err := s.store.verify(ctx, cred, password)
	if errors.Is(err, ErrHashOverBudget) {
		// The operator lowered the budget below an old hash's memory. Refuse
		// without counting a failure, at the cost and with the answer of a
		// wrong password, and tell the operator.
		s.store.hasher.VerifyDummy(password)
		s.logger.ErrorContext(ctx, "password login: stored hash exceeds the hashing memory budget; raise "+EnvMemoryBudgetKiB+" or reset this password",
			"user_id", util.UUIDToString(user.ID))
		return db.User{}, ErrInvalidCredentials
	}
	if err != nil {
		return db.User{}, err
	}
	if !ok {
		if err := s.recordFailure(ctx, user.ID, now); err != nil {
			return db.User{}, err
		}
		s.logger.WarnContext(ctx, "password login: wrong password", "user_id", util.UUIDToString(user.ID), "ip", ip)
		return db.User{}, ErrInvalidCredentials
	}
	if err := s.store.ResetFailures(ctx, user.ID); err != nil {
		return db.User{}, err
	}
	return user, nil
}

// failWithoutPassword answers an attempt on an email that has no password the
// way a wrong password on a real account would be answered, lockout included.
// It logs no identifier: the email is attacker input and may even be a
// password typed into the wrong field.
func (s *Service) failWithoutPassword(ctx context.Context, email, password, ip string) error {
	now := s.now()
	if s.phantoms.locked(email, now) {
		s.logger.WarnContext(ctx, "password login: account locked", "ip", ip)
		return ErrThrottled
	}
	s.store.hasher.VerifyDummy(password)
	s.phantoms.recordFailure(email, now)
	s.logger.WarnContext(ctx, "password login: no password for this email", "ip", ip)
	return ErrInvalidCredentials
}

// recordFailure counts one failure and locks the account once the count
// reaches LockThreshold.
func (s *Service) recordFailure(ctx context.Context, userID pgtype.UUID, now time.Time) error {
	n, err := s.store.RecordFailure(ctx, userID)
	if err != nil {
		return err
	}
	d := LockDuration(n)
	if d == 0 {
		return nil
	}
	if err := s.store.Lock(ctx, userID, now.Add(d)); err != nil {
		return err
	}
	s.logger.WarnContext(ctx, "password login: account locked after failures",
		"user_id", util.UUIDToString(userID), "failed_count", n, "locked_for", d.String())
	return nil
}

// HasPassword reports whether the user has set a password.
func (s *Service) HasPassword(ctx context.Context, userID pgtype.UUID) (bool, error) {
	_, err := s.store.Get(ctx, userID)
	if errors.Is(err, ErrNoCredential) {
		return false, nil
	}
	return err == nil, err
}

// SetPassword sets a password for an existing, signed-in user; the caller
// has checked both. The first password needs nothing more. Replacing one
// needs the current password, whose failures count towards the lockout.
func (s *Service) SetPassword(ctx context.Context, userID pgtype.UUID, current, next string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}

	cred, err := s.store.Get(ctx, userID)
	switch {
	case errors.Is(err, ErrNoCredential):
	case err != nil:
		return err
	default:
		if current == "" {
			return ErrCurrentPasswordRequired
		}
		now := s.now()
		if cred.Locked(now) {
			return ErrThrottled
		}
		ok, err := s.store.verify(ctx, cred, current)
		if err != nil {
			return err
		}
		if !ok {
			if err := s.recordFailure(ctx, userID, now); err != nil {
				return err
			}
			return ErrWrongCurrentPassword
		}
	}
	return s.store.SetPassword(ctx, userID, next)
}
