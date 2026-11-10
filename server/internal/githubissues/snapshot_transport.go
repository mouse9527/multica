package githubissues

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// errNotSnapshotRead refuses a request SnapshotTransport does not carry.
var errNotSnapshotRead = errors.New("githubissues: not a pull request snapshot read")

// snapshotReadPath matches the reads of ghsnapshot.FetchPRSnapshot, relative
// to the API root: a pull request, its head commit's check runs and statuses,
// and the workflow runs of a repository. It runs on the escaped path, so an
// encoded slash stays inside one segment and fails the name check.
var snapshotReadPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/(?:pulls/[0-9]+|commits/[0-9A-Fa-f]{7,64}/(?:check-runs|status)|actions/runs)$`)

// snapshotTransport adds the token to the PR snapshot reads of a ghsnapshot
// client. See SnapshotTransport.
type snapshotTransport struct {
	svc *Service
}

// SnapshotTransport returns the transport for ghsnapshot's static token
// source (ghsnapshot.Client.WithStaticSource), or nil on a nil Service. The
// token stays in this package: ghsnapshot builds unauthenticated requests and
// the transport adds the Authorization header to the ones it lets through.
//
// It lets through only a GET to the GitHub API for one of the reads
// FetchPRSnapshot makes, on a repository registered as a github_repo resource
// on a project, while the token is usable. The refresh queue does not know
// which workspace a pull request belongs to, so the allowlist is the union
// over all workspaces; whoever enqueues a pull request checks its workspace.
// Redirects are not followed here: the caller's client follows them through
// this transport again, so the target is checked like any other request.
// A 401 turns the token invalid, as it does for RepoClient.
func (s *Service) SnapshotTransport() http.RoundTripper {
	if s == nil {
		return nil
	}
	return snapshotTransport{svc: s}
}

func (t snapshotTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.svc.admitSnapshotRead(req); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.svc.sendWithToken(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		t.svc.markUnauthorized()
	}
	return resp, nil
}

// admitSnapshotRead returns nil when req may be sent with the token.
func (s *Service) admitSnapshotRead(req *http.Request) error {
	if req.Method != http.MethodGet {
		return errNotSnapshotRead
	}
	name, ok := s.snapshotReadRepo(req.URL)
	if !ok {
		return errNotSnapshotRead
	}
	if !s.Usable() {
		return ErrTokenNotUsable
	}
	if s.store == nil {
		return errors.New("githubissues: no database configured")
	}
	refs, err := s.store.ListGitHubRepoRefs(req.Context())
	if err != nil {
		return fmt.Errorf("githubissues: list registered repositories: %w", err)
	}
	for _, ref := range refs {
		if registered, ok := repoNameFromResourceRef(ref); ok && registered.sameAs(name) {
			return nil
		}
	}
	return ErrRepoNotRegistered
}

// snapshotReadRepo returns the repository u reads when u is a snapshot read
// under the API root.
func (s *Service) snapshotReadRepo(u *url.URL) (repoName, bool) {
	base, err := url.Parse(s.apiBase)
	if err != nil || u.User != nil || u.Opaque != "" || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
		return repoName{}, false
	}
	rest, ok := strings.CutPrefix(u.EscapedPath(), strings.TrimRight(base.EscapedPath(), "/"))
	if !ok {
		return repoName{}, false
	}
	m := snapshotReadPath.FindStringSubmatch(rest)
	if m == nil {
		return repoName{}, false
	}
	return newRepoName(m[1], m[2])
}
