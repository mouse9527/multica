package githubissues

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// revalidateInterval is the daily re-check after GitHub gave a verdict.
	revalidateInterval = 24 * time.Hour
	retryBaseDelay     = 30 * time.Second
	retryMaxDelay      = 10 * time.Minute
)

// Options are the injectable dependencies of a Service. Zero values select
// the production defaults.
type Options struct {
	HTTPClient *http.Client
	// APIBase is the GitHub REST API root; tests point it at a fake server.
	APIBase string
	Now     func() time.Time
	Logger  *slog.Logger
	// Queries backs RepoClient's allowlist and repository pins.
	Queries *db.Queries
}

// Service is the running GitHub-native issues feature. It is created only
// when the feature is enabled; a nil *Service means the feature is off.
type Service struct {
	cfg        Config
	httpClient *http.Client
	// repoHTTPClient is httpClient without redirects, for Repo.Do.
	repoHTTPClient *http.Client
	apiBase        string
	now            func() time.Time
	logger         *slog.Logger
	store          repoStore
	state          tokenState
	// failures counts consecutive inconclusive checks for the retry backoff.
	// Only the validation loop touches it.
	failures int
}

// New logs whether the feature is on and returns its Service, or nil when it
// is off. It makes no network call; Start validates the token.
func New(cfg Config, opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cfg.LogStatus(logger)
	if !cfg.Enabled() {
		return nil
	}
	s := &Service{
		cfg:        cfg,
		httpClient: opts.HTTPClient,
		apiBase:    strings.TrimRight(opts.APIBase, "/"),
		now:        opts.Now,
		logger:     logger,
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: requestTimeout}
	}
	if s.apiBase == "" {
		s.apiBase = defaultAPIBase
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.repoHTTPClient = noRedirectClient(s.httpClient)
	// A nil *db.Queries must stay a nil interface, not a typed nil.
	if opts.Queries != nil {
		s.store = opts.Queries
	}
	return s
}

// Start validates the token in the background, then re-validates it daily,
// until ctx is done. A failed validation never stops the server: the token
// stays invalid and the feature unusable until GitHub accepts it.
func (s *Service) Start(ctx context.Context) {
	go s.validateLoop(ctx)
}

func (s *Service) validateLoop(ctx context.Context) {
	for {
		timer := time.NewTimer(s.validate(ctx))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// validate checks the token once, records GitHub's verdict and returns how
// long to wait before the next check.
func (s *Service) validate(ctx context.Context) time.Duration {
	if s.cfg.isExpired(s.now()) {
		s.failures = 0
		s.logger.Warn("github token expired; github issues stopped",
			"token_expires_at", s.cfg.expiryString())
		return revalidateInterval
	}
	login, err := s.fetchLogin(ctx)
	var rejected *tokenRejectedError
	var limited *ghsnapshot.RateLimitError
	switch {
	case err == nil:
		s.failures = 0
		s.state.markAccepted(login, s.now())
		s.logger.Info("github token accepted", "account_login", login)
		return revalidateInterval
	case errors.As(err, &rejected):
		s.failures = 0
		s.state.markRejected(s.now())
		s.logger.Warn("github token invalid; github issues stopped", "category", rejected.category)
		return revalidateInterval
	case errors.As(err, &limited):
		s.logger.Warn("github token check rate limited", "retry_after", limited.RetryAfter)
		return limited.RetryAfter
	default:
		// Network errors and 5xx say nothing about the token: keep the last
		// verdict and ask again soon.
		delay := min(retryBaseDelay<<min(s.failures, 5), retryMaxDelay)
		s.failures++
		s.logger.Warn("github token check failed", "error", err, "retry_in", delay)
		return delay
	}
}
