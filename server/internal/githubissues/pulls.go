package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PullRequest is a pull request read from a registered repository's pulls
// list, normalized for the PR mirror.
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

// githubPull is the part of a pulls list item the poller reads. The list has
// no merged flag, line counts or mergeability: merged is merged_at being set,
// and the rest is left to the snapshot and the stored row.
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

// pullsResume is where a pulls read that ran out of budget continues: the
// next page, with the cursor it was reading down to and the newest updated_at
// it has seen. In memory only, like streamResume.
type pullsResume struct {
	stopAt time.Time
	page   int
	newest time.Time
}

type pullsResumeStore struct {
	mu sync.Mutex
	at map[streamKey]pullsResume
}

func (r *pullsResumeStore) get(key streamKey) (pullsResume, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.at[key]
	return at, ok
}

func (r *pullsResumeStore) set(key streamKey, at pullsResume) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at == nil {
		r.at = map[streamKey]pullsResume{}
	}
	r.at[key] = at
}

func (r *pullsResumeStore) clear(key streamKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.at, key)
}

// pollPulls reads the pull requests of a repository updated since cursor and
// hands each to the mirror.
//
// The pulls list has no "since", so it is read newest first
// (sort=updated&direction=desc) down to the first item older than cursor
// minus pollOverlap, or to its end on the first read. Only then does the
// cursor move, to the newest updated_at of the read: an item updated while the
// list is read moves to its front, which a later read covers, and the items
// behind it only move further back, so resuming at a page number re-reads
// some items but skips none. A read that runs out of pages, or stops at an
// item the mirror failed, resumes at that page next cycle.
func (s *Service) pollPulls(ctx context.Context, repo *Repo, cursor pgtype.Timestamptz) (synced int, err error) {
	var stopAt time.Time
	if cursor.Valid {
		stopAt = cursor.Time.Add(-pollOverlap)
	}
	key := streamKey{workspaceID: repo.workspaceID, repoID: repo.id, pulls: true}
	page := 1
	var newest time.Time
	if at, ok := s.pullsResume.get(key); ok && at.stopAt.Equal(stopAt) {
		page, newest = at.page, at.newest
	}
	perPage, pages := s.pollPageSize(), s.pollPageBudget()
	for range pages {
		query := url.Values{
			"state": {"all"}, "sort": {"updated"}, "direction": {"desc"},
			"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)},
		}
		resp, err := repo.Do(ctx, http.MethodGet, "/pulls?"+query.Encode(), nil)
		if err != nil {
			s.pullsResume.set(key, pullsResume{stopAt: stopAt, page: page, newest: newest})
			return synced, err
		}
		if resp.StatusCode != http.StatusOK {
			s.pullsResume.set(key, pullsResume{stopAt: stopAt, page: page, newest: newest})
			return synced, fmt.Errorf("github GET /pulls: unexpected status %d", resp.StatusCode)
		}
		var items []githubPull
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			s.pullsResume.set(key, pullsResume{stopAt: stopAt, page: page, newest: newest})
			return synced, errors.New("github GET /pulls: malformed response")
		}
		reached := len(items) < perPage
		for _, gh := range items {
			if gh.Number <= 0 || gh.UpdatedAt.IsZero() || (gh.State != "open" && gh.State != "closed") {
				s.pullsResume.set(key, pullsResume{stopAt: stopAt, page: page, newest: newest})
				return synced, errors.New("github GET /pulls: pull request without number, state or updated_at")
			}
			if !stopAt.IsZero() && gh.UpdatedAt.Before(stopAt) {
				reached = true
				break
			}
			changed, err := s.pullMirror.MirrorPolledPullRequest(ctx, repo.workspaceID, gh.normalized(repo.name))
			if err != nil {
				s.pullsResume.set(key, pullsResume{stopAt: stopAt, page: page, newest: newest})
				return synced, fmt.Errorf("mirror %s pull request %d: %w", repo.FullName(), gh.Number, err)
			}
			if changed {
				synced++
			}
			if gh.UpdatedAt.After(newest) {
				newest = gh.UpdatedAt
			}
		}
		if reached {
			s.pullsResume.clear(key)
			if newest.IsZero() {
				return synced, nil
			}
			q := db.New(s.db)
			if _, err := q.AdvanceGithubRepoSyncCursors(ctx, db.AdvanceGithubRepoSyncCursorsParams{
				WorkspaceID: repo.workspaceID, RepoID: repo.id,
				PullsCursor: pgtype.Timestamptz{Time: newest, Valid: true},
			}); err != nil {
				return synced, fmt.Errorf("githubissues: advance cursor: %w", err)
			}
			return synced, nil
		}
		page++
	}
	s.pullsResume.set(key, pullsResume{stopAt: stopAt, page: page, newest: newest})
	return synced, nil
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
