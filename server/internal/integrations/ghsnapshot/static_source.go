// Added by the mouse9527/multica fork: a static token source for Client, so
// pull requests read with the server's GitHub token
// (server/internal/githubissues) reuse the REST parsing, the refresh queue and
// the PR card of the GitHub App path unchanged.

package ghsnapshot

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// StaticInstallationID is the installation id of the static token source. It
// keys the refresh queue, the rate-limit pause and the PR rows like a real
// installation id does. GitHub installation ids are positive, so it never
// collides with one.
const StaticInstallationID int64 = -1

// WithStaticSource adds a static token source to c and returns c; on a nil c
// it returns a Client with only the static source. Requests for
// StaticInstallationID are sent through transport, which adds the
// credentials: the token itself never reaches this package. A nil transport
// returns c unchanged. Call it while building the client, before it is
// shared.
func (c *Client) WithStaticSource(transport http.RoundTripper) *Client {
	if transport == nil {
		return c
	}
	if c == nil {
		c = &Client{apiBase: defaultAPIBase, now: time.Now, tokens: map[int64]cachedToken{}}
	}
	c.static = &http.Client{Transport: transport, Timeout: 20 * time.Second}
	return c
}

// credentials returns the HTTP client and bearer token for installationID.
// The static source has no token here: its transport adds the credentials.
func (c *Client) credentials(ctx context.Context, installationID int64) (*http.Client, string, error) {
	if installationID == StaticInstallationID {
		if c.static == nil {
			return nil, "", errors.New("ghsnapshot: no static token source")
		}
		return c.static, "", nil
	}
	if c.privateKey == nil {
		return nil, "", errors.New("ghsnapshot: GitHub App not configured")
	}
	token, err := c.installationToken(ctx, installationID)
	if err != nil {
		return nil, "", err
	}
	return c.httpClient, token, nil
}
