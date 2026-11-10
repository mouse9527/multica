package githubissues

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// This file is the only place that builds a GitHub request or reads the
// token. scan_test.go fails the build if http.NewRequest, the token field or
// the Authorization header shows up anywhere else in the package, so every
// call to GitHub goes through RepoClient (or, for GET /user, the validation
// loop, and for PR snapshots, SnapshotTransport) and nothing outside the package can obtain the token or a client that
// carries it.

const apiVersion = "2022-11-28"

// newRequest builds an authenticated GitHub API request. body may be nil.
func (s *Service) newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		// The URL is built from the API base and validated names; dropping
		// the parse error keeps any surprise input out of logs anyway.
		return nil, errors.New("github: invalid request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// exchange sends an authenticated request and reads at most limit bytes of the
// response body. A body over the limit is an error. client selects the
// redirect policy; op names the call in errors ("GET /user"), which never
// carry the token or the body.
func (s *Service) exchange(ctx context.Context, client *http.Client, op, method, url string, body []byte, limit int64) (*http.Response, []byte, error) {
	req, err := s.newRequest(ctx, method, url, body)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github %s: %w", op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("github %s: failed to read response", op)
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("github %s: response exceeds size limit", op)
	}
	return resp, data, nil
}

// sendWithToken sends a copy of req with the token added and returns the
// response as it is, redirects included, for SnapshotTransport. req itself is
// never modified.
func (s *Service) sendWithToken(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+s.cfg.token)
	return s.repoHTTPClient.Do(out)
}

// noRedirectClient returns a client that hands redirects back to the caller.
// A repository addressed by id has no reason to redirect, and following one
// could turn a write into a GET or land on a different repository.
func noRedirectClient(base *http.Client) *http.Client {
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}
