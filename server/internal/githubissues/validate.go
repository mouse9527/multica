package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

const (
	defaultAPIBase = "https://api.github.com"
	requestTimeout = 15 * time.Second
	maxUserBody    = 1 << 20
	repoScope      = "repo"
	headerScopes   = "X-OAuth-Scopes"
	headerSSO      = "X-GitHub-SSO"
)

// Categories of a rejected token. They name the kind of rejection only; the
// response body is never kept, because it is not ours to log.
const (
	rejectUnauthorized     = "unauthorized"       // 401
	rejectSSORequired      = "sso_required"       // 403 with X-GitHub-SSO
	rejectForbidden        = "forbidden"          // any other non-throttling 403
	rejectMissingRepoScope = "missing_repo_scope" // X-OAuth-Scopes lacks repo
)

// tokenRejectedError means GitHub gave a verdict against the token. Retrying
// with the same token cannot succeed, unlike a network error or a 5xx.
type tokenRejectedError struct {
	category string
}

func (e *tokenRejectedError) Error() string {
	return "github token rejected: " + e.category
}

// classifyFailure maps a non-success GitHub response to a rate limit or a
// token rejection. Other failures return nil and are judged by status code.
func classifyFailure(resp *http.Response, body []byte, now time.Time) error {
	if ghsnapshot.IsRateLimited(resp, body) {
		return ghsnapshot.RateLimitFromResponse(resp, now)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return &tokenRejectedError{category: rejectUnauthorized}
	case http.StatusForbidden:
		if resp.Header.Get(headerSSO) != "" {
			return &tokenRejectedError{category: rejectSSORequired}
		}
		return &tokenRejectedError{category: rejectForbidden}
	}
	return nil
}

// lacksRepoScope reports whether GitHub listed the token's scopes without
// repo. Whether GET /user always sends X-OAuth-Scopes is not verified, so a
// missing header skips the check instead of failing it.
func lacksRepoScope(h http.Header) bool {
	values, present := h[http.CanonicalHeaderKey(headerScopes)]
	if !present {
		return false
	}
	for _, value := range values {
		for _, scope := range strings.Split(value, ",") {
			if strings.TrimSpace(scope) == repoScope {
				return false
			}
		}
	}
	return true
}

// fetchLogin calls GET /user with the token and returns the account login.
// Errors carry a status code or category, never the token or the body.
func (s *Service) fetchLogin(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github GET /user: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUserBody+1))
	if err != nil {
		return "", errors.New("github GET /user: failed to read response")
	}
	if err := classifyFailure(resp, body, s.now()); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github GET /user: unexpected status %d", resp.StatusCode)
	}
	if lacksRepoScope(resp.Header) {
		return "", &tokenRejectedError{category: rejectMissingRepoScope}
	}
	if len(body) > maxUserBody {
		return "", errors.New("github GET /user: response exceeds size limit")
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil || user.Login == "" {
		return "", errors.New("github GET /user: response has no login")
	}
	return user.Login, nil
}
