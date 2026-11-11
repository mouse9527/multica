package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PullRequest is a pull request read from a registered repository, normalized
// for the PR mirror.
type PullRequest struct {
	// Owner and Repo are the registered name of the repository, the name the
	// PR row, the snapshot reads and the allowlist all use.
	Owner, Repo string
	Number      int32
	HTMLURL     string
	Title, Body string
	// State is GitHub's "open" or "closed".
	State                        string
	Draft                        bool
	MergedAt, ClosedAt           *time.Time
	CreatedAt, UpdatedAt         time.Time
	HeadRef, HeadSHA             string
	AuthorLogin, AuthorAvatarURL string
}

// PullRequestMirror stores a polled pull request in a workspace: the handler's
// PR mirror, shared with the GitHub App webhook. It reports whether it wrote
// anything; an error leaves the pull request to be read again.
type PullRequestMirror interface {
	MirrorPolledPullRequest(ctx context.Context, workspaceID pgtype.UUID, pr PullRequest) (bool, error)
}

// SetPullRequestMirror turns pull request polling on. Call it before
// StartPolling; without a mirror the poller reads no pull requests.
func (s *Service) SetPullRequestMirror(m PullRequestMirror) {
	if s != nil {
		s.pullMirror = m
	}
}

// githubPull is the part of a pull request the poller reads. Merged is
// merged_at being set; line counts and mergeability are left to the snapshot
// and the stored row.
type githubPull struct {
	Number    int32      `json:"number"`
	HTMLURL   string     `json:"html_url"`
	Title     string     `json:"title"`
	Body      *string    `json:"body"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	User struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	Base struct {
		Repo struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repo"`
	} `json:"base"`
}

// errLinkedBudget ends a cycle's linked pull request scan of a repository
// when its request budget is spent; the scan continues there next cycle.
var errLinkedBudget = errors.New("githubissues: linked pull request budget spent")

// linkedResume is where the linked pull request scan of a repository
// continues after a cycle's budget ran out: the tracked issue it was reading
// and the timeline page it was on.
type linkedResume struct {
	number int32
	page   int
}

// linkedKey names one pull request seen on one tracked issue's timeline.
type linkedKey struct {
	issueID pgtype.UUID
	repoID  int64
	number  int32
}

// linkedPullsState is the in-memory state of the linked pull request scan,
// like streamResume: after a restart, or on another replica, every tracked
// issue is scanned and every pull request read once more.
type linkedPullsState struct {
	mu     sync.Mutex
	resume map[streamKey]linkedResume
	// scanned is each tracked issue's external_updated_at when its timeline
	// was last read to the end: a closed issue is read again only after it
	// changes.
	scanned map[pgtype.UUID]time.Time
	// mirrored is the updated_at a timeline showed for a pull request when it
	// was last mirrored from that issue's timeline: an unchanged pull request
	// is not read again.
	mirrored map[linkedKey]time.Time
}

func (l *linkedPullsState) getResume(key streamKey) (linkedResume, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.resume[key]
	return at, ok
}

func (l *linkedPullsState) setResume(key streamKey, at linkedResume) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resume == nil {
		l.resume = map[streamKey]linkedResume{}
	}
	l.resume[key] = at
}

func (l *linkedPullsState) clearResume(key streamKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.resume, key)
}

func (l *linkedPullsState) scannedAt(issueID pgtype.UUID) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.scanned[issueID]
	return at, ok
}

func (l *linkedPullsState) markScanned(issueID pgtype.UUID, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.scanned == nil {
		l.scanned = map[pgtype.UUID]time.Time{}
	}
	l.scanned[issueID] = at
}

func (l *linkedPullsState) mirroredAt(key linkedKey) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.mirrored[key]
	return at, ok
}

func (l *linkedPullsState) markMirrored(key linkedKey, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mirrored == nil {
		l.mirrored = map[linkedKey]time.Time{}
	}
	l.mirrored[key] = at
}

// timelineEvent is the part of an issue timeline event the poller reads: a
// "cross-referenced" event names the issue or pull request that mentioned
// this one, as it is now (state, title, body, updated_at), not as it was
// when it mentioned it.
type timelineEvent struct {
	Event  string `json:"event"`
	Source struct {
		Issue *timelineSource `json:"issue"`
	} `json:"source"`
}

type timelineSource struct {
	Number    int32     `json:"number"`
	Title     string    `json:"title"`
	Body      *string   `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
	// PullRequest is present when the source is a pull request.
	PullRequest json.RawMessage `json:"pull_request"`
	Repository  struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	RepositoryURL string `json:"repository_url"`
}

func (t timelineSource) isPull() bool {
	return len(t.PullRequest) > 0 && string(t.PullRequest) != "null"
}

// repo is the repository of the source, in GitHub's spelling.
func (t timelineSource) repo() (repoName, bool) {
	if t.Repository.Owner.Login != "" && t.Repository.Name != "" {
		return repoName{owner: t.Repository.Owner.Login, name: t.Repository.Name}, true
	}
	u, err := url.Parse(t.RepositoryURL)
	if err != nil {
		return repoName{}, false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/repos/"), "/")
	if !strings.HasPrefix(u.Path, "/repos/") || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return repoName{}, false
	}
	return repoName{owner: parts[0], name: parts[1]}, true
}

// pollLinkedPulls mirrors the pull requests that close the tracked issues of
// a repository, and no others: the repository's pull request list is never
// read, so what the poller sends grows with the tracked issues, not with the
// repository.
//
// It reads the timeline of each tracked issue that is open, or closed and
// changed since its timeline was last read to the end, and takes the pull
// requests in its "cross-referenced" events: GitHub records one whenever a
// pull request mentions the issue, "Closes #N" and "owner/repo#N" included,
// and shows the pull request as it is now. A pull request is read
// (GET /pulls/{n}) and mirrored only when its title or body closes a tracked
// issue of the workspace (PullRequestIssues, the rule its links follow), or
// when the poller mirrored it before, so that one that stopped claiming loses
// its link; and only when the timeline shows it updated since it was last
// mirrored from that issue.
//
// Timeline pages and pull request reads share one budget per repository per
// cycle (pollPageBudget). A scan that runs out continues next cycle at the
// issue and page it stopped on; a failed read or mirror stops it the same
// way, so the pull request is read again.
func (s *Service) pollLinkedPulls(ctx context.Context, repo *Repo) (synced int, err error) {
	tracked, err := db.New(s.db).ListTrackedGitHubIssuesInRepo(ctx, db.ListTrackedGitHubIssuesInRepoParams{
		WorkspaceID: repo.workspaceID, ExternalRepoID: repo.id,
	})
	if err != nil {
		return 0, fmt.Errorf("githubissues: list tracked issues: %w", err)
	}
	var due []db.ListTrackedGitHubIssuesInRepoRow
	for _, issue := range tracked {
		if at, ok := s.linkedPulls.scannedAt(issue.IssueID); issue.State == "open" || !ok || !at.Equal(issue.ExternalUpdatedAt.Time) {
			due = append(due, issue)
		}
	}
	key := streamKey{workspaceID: repo.workspaceID, repoID: repo.id, pulls: true}
	if len(due) == 0 {
		s.linkedPulls.clearResume(key)
		return 0, nil
	}
	start, page := 0, 1
	if at, ok := s.linkedPulls.getResume(key); ok {
		start = sort.Search(len(due), func(i int) bool { return due[i].ExternalNumber >= at.number }) % len(due)
		if due[start].ExternalNumber == at.number {
			page = at.page
		}
	}
	budget := s.pollPageBudget()
	for i := range due {
		issue := due[(start+i)%len(due)]
		if i > 0 {
			page = 1
		}
		for {
			n, more, err := s.scanTimelinePage(ctx, repo, issue, page, &budget)
			synced += n
			if err != nil {
				s.linkedPulls.setResume(key, linkedResume{number: issue.ExternalNumber, page: page})
				if errors.Is(err, errLinkedBudget) {
					return synced, nil
				}
				return synced, err
			}
			if !more {
				break
			}
			page++
		}
		s.linkedPulls.markScanned(issue.IssueID, issue.ExternalUpdatedAt.Time)
	}
	s.linkedPulls.clearResume(key)
	return synced, nil
}

// scanTimelinePage reads one page of a tracked issue's timeline and mirrors
// the pull requests on it. more is true when the page was full.
func (s *Service) scanTimelinePage(ctx context.Context, repo *Repo, issue db.ListTrackedGitHubIssuesInRepoRow, page int, budget *int) (synced int, more bool, err error) {
	if *budget <= 0 {
		return 0, false, errLinkedBudget
	}
	*budget--
	perPage := s.pollPageSize()
	query := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
	resp, err := repo.Do(ctx, http.MethodGet, fmt.Sprintf("/issues/%d/timeline?%s", issue.ExternalNumber, query.Encode()), nil)
	if err != nil {
		return 0, false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusMovedPermanently, http.StatusNotFound, http.StatusGone:
		// Deleted or transferred: the refresh of the issue deals with it.
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("github GET /issues/{n}/timeline: unexpected status %d", resp.StatusCode)
	}
	var events []timelineEvent
	if err := json.Unmarshal(resp.Body, &events); err != nil {
		return 0, false, errors.New("github GET /issues/{n}/timeline: malformed response")
	}
	for _, ev := range events {
		if ev.Event != "cross-referenced" || ev.Source.Issue == nil || !ev.Source.Issue.isPull() {
			continue
		}
		changed, err := s.mirrorLinkedPull(ctx, repo, issue.IssueID, *ev.Source.Issue, budget)
		if err != nil {
			return synced, false, err
		}
		if changed {
			synced++
		}
	}
	return synced, len(events) >= perPage, nil
}

// mirrorLinkedPull reads and mirrors one pull request seen on the timeline of
// tracked issue issueID, if it closes a tracked issue (see pollLinkedPulls).
// A pull request in a repository the workspace has not registered is left
// alone without a request.
func (s *Service) mirrorLinkedPull(ctx context.Context, repo *Repo, issueID pgtype.UUID, src timelineSource, budget *int) (bool, error) {
	name, ok := src.repo()
	if src.Number <= 0 || !ok {
		return false, nil
	}
	// A repository renamed since it was registered is still the same id.
	prRepo := repo
	if src.Repository.ID != repo.id && !name.sameAs(repo.name) {
		other, err := s.RepoClient(ctx, repo.workspaceID, name.owner, name.name)
		switch {
		case errors.Is(err, ErrRepoNotRegistered), errors.Is(err, ErrRepoAmbiguous):
			return false, nil
		case err != nil:
			return false, err
		}
		prRepo = other
	}
	key := linkedKey{issueID: issueID, repoID: prRepo.id, number: src.Number}
	if at, ok := s.linkedPulls.mirroredAt(key); ok && at.Equal(src.UpdatedAt) {
		return false, nil
	}
	claims, err := s.linkedPullClaims(ctx, prRepo, name, src)
	if err != nil || !claims {
		return false, err
	}
	if *budget <= 0 {
		return false, errLinkedBudget
	}
	*budget--
	resp, err := prRepo.Do(ctx, http.MethodGet, fmt.Sprintf("/pulls/%d", src.Number), nil)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusMovedPermanently, http.StatusNotFound, http.StatusGone:
		return false, nil
	default:
		return false, fmt.Errorf("github GET /pulls/{n}: unexpected status %d", resp.StatusCode)
	}
	var gh githubPull
	if err := json.Unmarshal(resp.Body, &gh); err != nil {
		return false, errors.New("github GET /pulls/{n}: malformed response")
	}
	if gh.Number != src.Number || gh.UpdatedAt.IsZero() || (gh.State != "open" && gh.State != "closed") {
		return false, errors.New("github GET /pulls/{n}: pull request without number, state or updated_at")
	}
	changed, err := s.pullMirror.MirrorPolledPullRequest(ctx, repo.workspaceID, gh.normalized(prRepo.name))
	if err != nil {
		return false, fmt.Errorf("mirror %s pull request %d: %w", prRepo.FullName(), gh.Number, err)
	}
	s.linkedPulls.markMirrored(key, src.UpdatedAt)
	return changed, nil
}

// linkedPullClaims reports whether a pull request from a timeline is one the
// poller mirrors: it closes a tracked issue of the workspace, or the poller
// mirrored it before. name is its repository in GitHub's spelling, which a
// mirrored row uses when it differs from the registered name only in case
// (githubPull.normalized).
func (s *Service) linkedPullClaims(ctx context.Context, prRepo *Repo, name repoName, src timelineSource) (bool, error) {
	body := ""
	if src.Body != nil {
		body = *src.Body
	}
	ids, err := s.PullRequestIssues(ctx, prRepo.workspaceID, prRepo.name.owner, prRepo.name.name, src.Title, body)
	if err != nil || len(ids) > 0 {
		return len(ids) > 0, err
	}
	row, err := db.New(s.db).GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: prRepo.workspaceID, RepoOwner: name.owner, RepoName: name.name, PrNumber: src.Number,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("githubissues: read mirrored pull request: %w", err)
	}
	return row.InstallationID == ghsnapshot.StaticInstallationID, nil
}

// normalized names the repository as registered, in GitHub's spelling when
// the two differ only in case, so the PR row is the one the App webhook would
// write for it. A repository renamed since keeps its registered name, the one
// the snapshot allowlist accepts.
func (g githubPull) normalized(name repoName) PullRequest {
	if base := (repoName{owner: g.Base.Repo.Owner.Login, name: g.Base.Repo.Name}); base.sameAs(name) {
		name = base
	}
	return PullRequest{
		Owner: name.owner, Repo: name.name, Number: g.Number, HTMLURL: g.HTMLURL,
		Title: g.Title, Body: stringValue(g.Body), State: g.State, Draft: g.Draft,
		MergedAt: g.MergedAt, ClosedAt: g.ClosedAt, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
		HeadRef: g.Head.Ref, HeadSHA: g.Head.SHA, AuthorLogin: g.User.Login, AuthorAvatarURL: g.User.AvatarURL,
	}
}

// closingRefRe matches a GitHub issue reference right after a closing
// keyword, the way GitHub links one: "Closes #12", "fixes: acme/widgets#12".
// A bare "#12" claims nothing: squash-merge titles end in "(#<pr number>)".
var closingRefRe = regexp.MustCompile(
	`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)[:\s]+(?:([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100}))?#([0-9]{1,10})\b`,
)

// issueRef is one GitHub issue a pull request claims; an empty owner means
// the pull request's own repository.
type issueRef struct {
	owner, name string
	number      int32
}

func closingRefs(parts ...string) []issueRef {
	var refs []issueRef
	for _, part := range parts {
		for _, m := range closingRefRe.FindAllStringSubmatch(part, -1) {
			n, err := strconv.ParseInt(m[3], 10, 32)
			if err != nil || n <= 0 {
				continue
			}
			refs = append(refs, issueRef{owner: m[1], name: m[2], number: int32(n)})
		}
	}
	return refs
}

// PullRequestIssues returns the GitHub-native issues of the workspace a pull
// request in owner/repo claims with a closing keyword in its title or body:
// "#N" in the same repository, "owner/name#N" in any repository the workspace
// registered. A reference is matched by (repository id, number) through the
// workspace's repository pins and issue_external, never by the workspace's
// issue prefix. References that match no imported issue are skipped.
func (s *Service) PullRequestIssues(ctx context.Context, workspaceID pgtype.UUID, owner, repo, title, body string) ([]pgtype.UUID, error) {
	if s == nil {
		return nil, ErrFeatureOff
	}
	refs := closingRefs(title, body)
	if len(refs) == 0 {
		return nil, nil
	}
	if s.store == nil || s.db == nil {
		return nil, ErrNoDatabase
	}
	pins, err := s.store.ListGithubRepoSyncsByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("githubissues: list pinned repositories: %w", err)
	}
	self := repoName{owner: owner, name: repo}
	q := db.New(s.db)
	var ids []pgtype.UUID
	seen := map[pgtype.UUID]bool{}
	for _, ref := range refs {
		target := self
		if ref.owner != "" {
			target = repoName{owner: ref.owner, name: ref.name}
		}
		var repoID int64
		for _, pin := range pins {
			if target.sameAs(repoName{owner: pin.Owner, name: pin.Name}) {
				repoID = pin.RepoID
			}
		}
		if repoID == 0 {
			continue
		}
		ext, err := q.GetIssueExternalByNumber(ctx, db.GetIssueExternalByNumberParams{
			WorkspaceID: workspaceID, Provider: issuekey.ProviderGitHub,
			ExternalRepoID: repoID, ExternalNumber: ref.number,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("githubissues: look up issue %s#%d: %w", target, ref.number, err)
		}
		if !seen[ext.IssueID] {
			seen[ext.IssueID] = true
			ids = append(ids, ext.IssueID)
		}
	}
	return ids, nil
}
