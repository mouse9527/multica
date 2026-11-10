package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	maxRepoBody     = 1 << 20
	maxResponseBody = 8 << 20
)

var (
	// ErrFeatureOff is returned by RepoClient on a nil Service.
	ErrFeatureOff = errors.New("githubissues: feature is off")
	// ErrRepoNotRegistered means no project of the workspace has the
	// repository as a github_repo resource. No request was sent.
	ErrRepoNotRegistered = errors.New("githubissues: repository is not registered in this workspace")
	// ErrTokenNotUsable means the token is invalid or expired. No request
	// was sent, or GitHub has just rejected the token.
	ErrTokenNotUsable = errors.New("githubissues: github token is not usable")
	// ErrRepoAmbiguous means the name resolves to a repository that is
	// already pinned under another name the workspace still registers (for
	// example both the old and the new name after a rename). The existing
	// pin is kept and this name is refused until one of the two resources
	// is removed.
	ErrRepoAmbiguous = errors.New("githubissues: repository is registered under another name in this workspace")
)

// Is lets callers outside the package test a rejection with
// errors.Is(err, ErrTokenNotUsable).
func (e *tokenRejectedError) Is(target error) bool { return target == ErrTokenNotUsable }

// repoStore is the part of *db.Queries RepoClient needs.
type repoStore interface {
	ListWorkspaceGitHubRepoRefs(ctx context.Context, workspaceID pgtype.UUID) ([][]byte, error)
	ListGithubRepoSyncsByWorkspace(ctx context.Context, workspaceID pgtype.UUID) ([]db.GithubRepoSync, error)
	InsertGithubRepoPin(ctx context.Context, arg db.InsertGithubRepoPinParams) (db.GithubRepoSync, error)
	MoveGithubRepoPin(ctx context.Context, arg db.MoveGithubRepoPinParams) (db.GithubRepoSync, error)
}

// pinAttempts bounds how often RepoClient re-reads the pins after losing a
// race to another caller. Each lost race means some pin changed, and the
// next read sees it, so a second attempt already settles; the bound only
// guards against a store that keeps changing underneath.
const pinAttempts = 3

// Repo is a GitHub repository the workspace has registered, pinned to its
// GitHub repository id. Every request goes to /repositories/{id}/..., so a
// rename or transfer keeps reaching the same repository and a new repository
// created under the old name is never reached.
//
// Repo carries no token and exposes no HTTP client; Do is the only way to
// call GitHub through it.
type Repo struct {
	svc         *Service
	workspaceID pgtype.UUID
	id          int64
	name        repoName
}

// RepoClient returns the client for owner/repo in the workspace. It is the
// single gate to GitHub repositories: polling, on-demand refresh, writes and
// PR polling all go through it.
//
// The allowlist is the union of the github_repo resources on the workspace's
// projects. A repository outside it gets ErrRepoNotRegistered without any
// request. The first use of a registered name resolves it to a repository id
// with GET /repos/{owner}/{repo} and pins the id in github_repo_sync; later
// calls use the pinned id and send nothing until Do. A pin row holds one name,
// so a pin whose name is still registered is never taken over by another
// name, not even by a concurrent first use: that name gets ErrRepoAmbiguous.
func (s *Service) RepoClient(ctx context.Context, workspaceID pgtype.UUID, owner, repo string) (*Repo, error) {
	if s == nil {
		return nil, ErrFeatureOff
	}
	if s.store == nil {
		return nil, errors.New("githubissues: no database configured")
	}
	name, ok := newRepoName(owner, repo)
	if !ok {
		return nil, ErrRepoNotRegistered
	}
	registered, err := s.registeredNames(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !containsName(registered, name) {
		return nil, ErrRepoNotRegistered
	}
	if !s.Usable() {
		return nil, ErrTokenNotUsable
	}

	// The pins are read, judged and then written with a conditional write:
	// a new pin only if the id has none, a moved pin only if it still has
	// the name read here. A write that finds the pin changed returns no row,
	// and the loop judges again from a fresh read, so between concurrent
	// callers the first write wins and the others see it.
	var id int64
	for attempt := 1; ; attempt++ {
		pins, err := s.store.ListGithubRepoSyncsByWorkspace(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("githubissues: list pinned repositories: %w", err)
		}
		for _, pin := range pins {
			if name.sameAs(repoName{owner: pin.Owner, name: pin.Name}) {
				return &Repo{svc: s, workspaceID: workspaceID, id: pin.RepoID, name: name}, nil
			}
		}
		if id == 0 {
			if id, err = s.resolveRepoID(ctx, name); err != nil {
				return nil, err
			}
		}

		var current *db.GithubRepoSync
		for i := range pins {
			if pins[i].RepoID == id {
				current = &pins[i]
			}
		}
		if current == nil {
			_, err = s.store.InsertGithubRepoPin(ctx, db.InsertGithubRepoPinParams{
				WorkspaceID: workspaceID, RepoID: id, Owner: name.owner, Name: name.name,
			})
		} else {
			pinned := repoName{owner: current.Owner, name: current.Name}
			if containsName(registered, pinned) {
				s.logger.Warn("github repository registered under two names; refusing the unpinned one",
					"repo", name.String(), "pinned_as", pinned.String(), "repo_id", id)
				return nil, ErrRepoAmbiguous
			}
			// The pinned name is no longer registered: the resource moved to
			// this name, and the row keeps its cursors.
			_, err = s.store.MoveGithubRepoPin(ctx, db.MoveGithubRepoPinParams{
				WorkspaceID: workspaceID, RepoID: id, Owner: name.owner, Name: name.name,
				FromOwner: current.Owner, FromName: current.Name,
			})
		}
		switch {
		case err == nil:
			s.logger.Info("github repository pinned", "repo", name.String(), "repo_id", id)
			return &Repo{svc: s, workspaceID: workspaceID, id: id, name: name}, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("githubissues: pin repository: %w", err)
		case attempt == pinAttempts:
			return nil, errors.New("githubissues: pin repository: pins kept changing concurrently")
		}
	}
}

// registeredNames lists the github.com repositories registered on the
// workspace's projects.
func (s *Service) registeredNames(ctx context.Context, workspaceID pgtype.UUID) ([]repoName, error) {
	refs, err := s.store.ListWorkspaceGitHubRepoRefs(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("githubissues: list registered repositories: %w", err)
	}
	names := make([]repoName, 0, len(refs))
	for _, ref := range refs {
		if name, ok := repoNameFromResourceRef(ref); ok {
			names = append(names, name)
		}
	}
	return names, nil
}

func containsName(names []repoName, name repoName) bool {
	for _, candidate := range names {
		if candidate.sameAs(name) {
			return true
		}
	}
	return false
}

// resolveRepoID asks GitHub which repository a registered name is. A renamed
// repository answers its old name with a redirect, which is followed here so
// a resource still holding the old name pins the same id.
func (s *Service) resolveRepoID(ctx context.Context, name repoName) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, body, err := s.exchange(ctx, s.httpClient, "GET /repos", http.MethodGet,
		s.apiBase+"/repos/"+name.owner+"/"+name.name, nil, maxRepoBody)
	if err != nil {
		return 0, err
	}
	if err := s.checkResponse(resp, body); err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github GET /repos: unexpected status %d", resp.StatusCode)
	}
	var repo struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &repo); err != nil || repo.ID <= 0 {
		return 0, errors.New("github GET /repos: response has no repository id")
	}
	return repo.ID, nil
}

// checkResponse turns throttling and a rejected token into errors. A 401
// is GitHub's verdict on the token, so the token turns invalid and every
// RepoClient stops until the next validation accepts it again. A 403 that is
// not throttling is left to the caller: on a repository endpoint it can be a
// per-repository refusal (SSO, access) rather than a bad token.
func (s *Service) checkResponse(resp *http.Response, body []byte) error {
	if ghsnapshot.IsRateLimited(resp, body) {
		return ghsnapshot.RateLimitFromResponse(resp, s.now())
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.state.markRejected(s.now())
		s.logger.Warn("github token invalid; github issues stopped", "category", rejectUnauthorized)
		return &tokenRejectedError{category: rejectUnauthorized}
	}
	return nil
}

// Response is a GitHub answer to Repo.Do. Status codes other than 401 and
// throttling are returned as they are, for the caller to judge.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Do sends method to path under the pinned repository, for example
// Do(ctx, "GET", "/issues?state=all", nil) for /repositories/{id}/issues.
// body, when not nil, is sent as JSON. Redirects are not followed.
//
// Errors: ErrTokenNotUsable (checked before sending, or a 401),
// *ghsnapshot.RateLimitError when throttled, or a transport error.
func (r *Repo) Do(ctx context.Context, method, path string, body []byte) (*Response, error) {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		return nil, fmt.Errorf("githubissues: method %q not allowed", method)
	}
	cleanPath, ok := repoRequestPath(path)
	if !ok {
		return nil, errors.New("githubissues: invalid repository request path")
	}
	if !r.svc.Usable() {
		return nil, ErrTokenNotUsable
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	base := "/repositories/" + strconv.FormatInt(r.id, 10)
	resp, data, err := r.svc.exchange(ctx, r.svc.repoHTTPClient, method+" "+base+cleanPath.Path, method,
		r.svc.apiBase+base+cleanPath.String(), body, maxResponseBody)
	if err != nil {
		return nil, err
	}
	if err := r.svc.checkResponse(resp, data); err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: data}, nil
}

// repoRequestPath accepts "" or an absolute path with an optional query, and
// nothing that could leave the repository: no scheme, host, fragment, dot
// segments or backslashes.
func repoRequestPath(path string) (*url.URL, bool) {
	if path == "" {
		return &url.URL{}, true
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, `\`) {
		return nil, false
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return nil, false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, false
		}
	}
	return u, true
}

// ID is the pinned GitHub repository id.
func (r *Repo) ID() int64 { return r.id }

// FullName is the registered "owner/name" the client was asked for.
func (r *Repo) FullName() string { return r.name.String() }

// String renders the repository without the service it belongs to. The
// formatting methods have value receivers so a copied Repo is covered too.
func (r Repo) String() string {
	return "githubissues.Repo{repo_id: " + strconv.FormatInt(r.id, 10) + ", name: " + r.name.String() + "}"
}

// GoString keeps %#v from walking into the service.
func (r Repo) GoString() string { return r.String() }

// Format routes every fmt verb through String: fmt cannot call the
// service's own formatting on an unexported field, so it would print the
// token.
func (r Repo) Format(f fmt.State, verb rune) { formatAs(f, verb, r.String()) }

// LogValue renders the repository for slog.
func (r Repo) LogValue() slog.Value {
	return slog.GroupValue(slog.Int64("repo_id", r.id), slog.String("repo", r.name.String()))
}

// String renders the service as its configuration, which omits the token.
func (s *Service) String() string { return "githubissues.Service{" + s.cfg.String() + "}" }

// GoString keeps %#v from dumping the unexported fields.
func (s *Service) GoString() string { return s.String() }

// Format routes every fmt verb through String, for the same reason as Repo.
func (s *Service) Format(f fmt.State, verb rune) { formatAs(f, verb, s.String()) }

// LogValue renders the service for slog without the token.
func (s *Service) LogValue() slog.Value { return s.cfg.LogValue() }

func formatAs(f fmt.State, verb rune, s string) {
	if verb == 'q' {
		fmt.Fprintf(f, "%q", s)
		return
	}
	fmt.Fprint(f, s)
}
