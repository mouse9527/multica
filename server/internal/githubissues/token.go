package githubissues

import (
	"sync"
	"time"
)

// TokenStatus is the state of the configured token.
type TokenStatus string

const (
	// TokenOK means GitHub accepted the token at the last check.
	TokenOK TokenStatus = "ok"
	// TokenInvalid means GitHub rejected the token, or it has not been
	// accepted yet since this process started.
	TokenInvalid TokenStatus = "invalid"
	// TokenExpired means the registered expiry date has been reached.
	TokenExpired TokenStatus = "expired"
)

// TokenStatusResponse is the read-only status of the token. It deliberately
// has no field that could carry the token or any part of it.
type TokenStatusResponse struct {
	// AccountLogin is the GitHub login the token belongs to; null until
	// GitHub has accepted the token once.
	AccountLogin *string     `json:"account_login"`
	Status       TokenStatus `json:"status"`
	// ExpiresAt is the registered expiry date (YYYY-MM-DD); null when the
	// token never expires.
	ExpiresAt *string `json:"expires_at"`
	// ValidatedAt is when GitHub last gave a verdict (accepted or rejected)
	// on the token; null before the first verdict.
	ValidatedAt *time.Time `json:"validated_at"`
}

// tokenState is this process's view of the token. Every server process
// validates for itself, so replicas never depend on each other's memory.
type tokenState struct {
	mu           sync.Mutex
	accepted     bool
	accountLogin string
	validatedAt  time.Time
}

func (s *tokenState) markAccepted(login string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = true
	s.accountLogin = login
	s.validatedAt = at
}

// login is the account GitHub last accepted the token for; empty before the
// first acceptance.
func (s *tokenState) login() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountLogin
}

func (s *tokenState) markRejected(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = false
	s.validatedAt = at
}

// isExpired reports whether the token is past its registered expiry date. The
// expiry date itself already counts as expired, so the feature stops before
// GitHub starts rejecting the token rather than after.
func (c Config) isExpired(now time.Time) bool {
	expiresAt, ok := c.TokenExpiresAt()
	return ok && !now.Before(expiresAt)
}

func (s *Service) tokenStatus(now time.Time) TokenStatusResponse {
	s.state.mu.Lock()
	accepted, login, validatedAt := s.state.accepted, s.state.accountLogin, s.state.validatedAt
	s.state.mu.Unlock()

	out := TokenStatusResponse{Status: TokenInvalid}
	switch {
	case s.cfg.isExpired(now):
		out.Status = TokenExpired
	case accepted:
		out.Status = TokenOK
	}
	if login != "" {
		out.AccountLogin = &login
	}
	if _, ok := s.cfg.TokenExpiresAt(); ok {
		expiry := s.cfg.expiryString()
		out.ExpiresAt = &expiry
	}
	if !validatedAt.IsZero() {
		at := validatedAt.UTC()
		out.ValidatedAt = &at
	}
	return out
}

// TokenStatus returns the current status of the token.
func (s *Service) TokenStatus() TokenStatusResponse {
	return s.tokenStatus(s.now())
}

// Usable reports whether GitHub may be called with the token: it has been
// accepted and has not expired. Polling and writes must stop while it is
// false.
func (s *Service) Usable() bool {
	return s.TokenStatus().Status == TokenOK
}
