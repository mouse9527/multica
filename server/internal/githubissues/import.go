package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	// ErrInvalidIssueRef: the reference is neither "owner/repo#N" nor the
	// URL of a GitHub issue.
	ErrInvalidIssueRef = errors.New("githubissues: not a GitHub issue reference (owner/repo#N or https://github.com/owner/repo/issues/N)")
	// ErrPullRequest: the number is a pull request, which is not imported
	// as an issue.
	ErrPullRequest = errors.New("githubissues: the number is a pull request, not an issue")
	// ErrGitHubIssueNotFound: GitHub has no issue with that number in the
	// repository, or it was deleted or transferred.
	ErrGitHubIssueNotFound = errors.New("githubissues: issue not found on GitHub")
	// ErrImportPaused: GitHub throttled this server and the wait it asked
	// for has not passed.
	ErrImportPaused = errors.New("githubissues: GitHub asked to wait; try again later")
	// ErrImportIncomplete: the issue is stored but not all of its comments
	// were read. Importing it again reads them all once more. ImportIssue
	// wraps it together with the cause, so a rate limit is still told apart.
	ErrImportIncomplete = errors.New("githubissues: the issue is imported but not all of its comments were read; import it again to finish")
)

// importCommentPages bounds the comment pages one import reads, as a guard
// against a list that never ends; it is far more comments than one issue
// holds. An issue with more is an incomplete import.
const importCommentPages = 1000

// IssueRef names one GitHub issue.
type IssueRef struct {
	Owner  string
	Repo   string
	Number int32
}

func (r IssueRef) String() string { return r.Owner + "/" + r.Repo + "#" + strconv.Itoa(int(r.Number)) }

// ParseIssueRef reads "owner/repo#N" or the URL of a GitHub issue,
// https://github.com/owner/repo/issues/N (a query or fragment, such as a
// link to one comment, is ignored). A pull request URL is ErrPullRequest; a
// pull request number written as owner/repo#N is only told apart by GitHub,
// in ImportIssue.
func ParseIssueRef(raw string) (IssueRef, error) {
	raw = strings.TrimSpace(raw)
	if repo, number, ok := issuekey.ParseGitHub(raw); ok {
		owner, name, _ := strings.Cut(repo, "/")
		return IssueRef{Owner: owner, Repo: name, Number: number}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return IssueRef{}, ErrInvalidIssueRef
	}
	if host := strings.ToLower(u.Host); host != "github.com" && host != "www.github.com" {
		return IssueRef{}, ErrInvalidIssueRef
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	if len(parts) != 4 {
		return IssueRef{}, ErrInvalidIssueRef
	}
	name, ok := newRepoName(parts[0], parts[1])
	n, err := strconv.ParseInt(parts[3], 10, 32)
	if !ok || err != nil || n <= 0 {
		return IssueRef{}, ErrInvalidIssueRef
	}
	switch parts[2] {
	case "issues":
		return IssueRef{Owner: name.owner, Repo: name.name, Number: int32(n)}, nil
	case "pull", "pulls":
		return IssueRef{}, ErrPullRequest
	}
	return IssueRef{}, ErrInvalidIssueRef
}

// ImportIssue brings one GitHub issue into Multica on request, after which
// the poller and the on-view refresh follow it. Nothing else enters Multica
// from GitHub: issues nobody imported stay out however often they change.
//
// The repository must be registered in the workspace (RepoClient). An issue
// already imported is returned with created false, after its comments are
// read again (below). Otherwise the issue and its comments are read, the issue is
// stored as the poller stored issues opened on GitHub (the sync account as
// creator, the GitHub author in the description prefix, nobody assigned, the
// project that registered the repository first), and created is true unless
// a concurrent import or poll stored it first.
//
// Every comment of the issue is read: the poller only reads comments updated
// after the repository's cursor, so none older would arrive later. When they
// cannot all be read (GitHub failed or throttled midway) the issue is still
// stored and returned with an error wrapping ErrImportIncomplete. Importing
// an issue that is already imported reads its comments again, which is how
// such an import is finished; it never creates a second issue.
func (s *Service) ImportIssue(ctx context.Context, workspaceID pgtype.UUID, ref IssueRef) (issueID pgtype.UUID, created bool, err error) {
	if s == nil {
		return pgtype.UUID{}, false, ErrFeatureOff
	}
	if s.db == nil {
		return pgtype.UUID{}, false, ErrNoDatabase
	}
	q := db.New(s.db)
	// The registered spelling, so the issue is named as the poller names it.
	name, projectID, err := importTarget(ctx, q, workspaceID, ref)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	repo, err := s.RepoClient(ctx, workspaceID, name.owner, name.name)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	id, found, err := importedIssue(ctx, q, repo, ref.Number)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if s.pollGate.paused(s.now()) {
		return id, false, ErrImportPaused
	}
	if found {
		return id, false, s.importComments(ctx, repo, id, ref.Number)
	}
	// Started before the issue is read, so an edit GitHub takes after the
	// read falls inside the poller's window.
	if _, err := s.startCursors(ctx, repo); err != nil {
		return pgtype.UUID{}, false, err
	}

	path := "/issues/" + strconv.Itoa(int(ref.Number))
	resp, err := repo.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		s.pauseIfThrottled(err)
		return pgtype.UUID{}, false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone, http.StatusMovedPermanently:
		return pgtype.UUID{}, false, ErrGitHubIssueNotFound
	default:
		return pgtype.UUID{}, false, fmt.Errorf("github GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var gh githubIssue
	if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.Number != ref.Number {
		return pgtype.UUID{}, false, fmt.Errorf("github GET %s: malformed issue", path)
	}
	if len(gh.PullRequest) > 0 && string(gh.PullRequest) != "null" {
		return pgtype.UUID{}, false, ErrPullRequest
	}

	created, _, err = s.storeIssue(ctx, repo, projectID, gh, true)
	if err != nil {
		return pgtype.UUID{}, false, fmt.Errorf("import %s#%d: %w", repo.FullName(), ref.Number, err)
	}
	id, found, err = importedIssue(ctx, q, repo, ref.Number)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if !found {
		// Deleted locally between the store and this read.
		return pgtype.UUID{}, false, fmt.Errorf("import %s#%d: issue not stored", repo.FullName(), ref.Number)
	}
	return id, created, s.importComments(ctx, repo, id, ref.Number)
}

// importComments reads every comment of an imported issue. A failure, or a
// list longer than importCommentPages, is an error wrapping
// ErrImportIncomplete.
func (s *Service) importComments(ctx context.Context, repo *Repo, issueID pgtype.UUID, number int32) error {
	complete, _, err := s.syncIssueComments(ctx, repo, issueID, number, importCommentPages)
	if err == nil && complete {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("more than %d pages of comments", importCommentPages)
	}
	s.pauseIfThrottled(err)
	s.logger.Warn("github issue import: not all comments stored", "repo", repo, "issue_number", number,
		"issue_id", util.UUIDToString(issueID), "error", err)
	return fmt.Errorf("%w: %w", ErrImportIncomplete, err)
}

// importedIssue finds the local issue that mirrors GitHub issue number.
func importedIssue(ctx context.Context, q *db.Queries, repo *Repo, number int32) (pgtype.UUID, bool, error) {
	ext, err := q.GetIssueExternalByNumber(ctx, db.GetIssueExternalByNumberParams{
		WorkspaceID: repo.workspaceID, Provider: issuekey.ProviderGitHub,
		ExternalRepoID: repo.id, ExternalNumber: number,
	})
	switch {
	case err == nil:
		return ext.IssueID, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return pgtype.UUID{}, false, nil
	default:
		return pgtype.UUID{}, false, err
	}
}

// importTarget finds the repository of ref among those registered in the
// workspace: its registered name, and the project that registered it first,
// where the issue lands. RepoClient checks the allowlist again.
func importTarget(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, ref IssueRef) (repoName, pgtype.UUID, error) {
	want, ok := newRepoName(ref.Owner, ref.Repo)
	if !ok {
		return repoName{}, pgtype.UUID{}, ErrRepoNotRegistered
	}
	resources, err := q.ListWorkspaceGitHubRepoResources(ctx, workspaceID)
	if err != nil {
		return repoName{}, pgtype.UUID{}, fmt.Errorf("githubissues: list registered repositories: %w", err)
	}
	for _, res := range resources {
		if name, ok := repoNameFromResourceRef(res.ResourceRef); ok && name.sameAs(want) {
			return name, res.ProjectID, nil
		}
	}
	return repoName{}, pgtype.UUID{}, ErrRepoNotRegistered
}

// pauseIfThrottled pauses polling, on-view refreshes and imports for as long
// as GitHub asked when err is a rate limit.
func (s *Service) pauseIfThrottled(err error) {
	var limited *ghsnapshot.RateLimitError
	if errors.As(err, &limited) {
		s.pollGate.pause(s.now().Add(limited.RetryAfter))
	}
}
