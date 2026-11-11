package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func (f *fakeIssuesAPI) putPull(number int, title, body, state string, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[number] = map[string]any{
		"number": number, "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"title": title, "body": body, "state": state, "draft": false, "merged_at": nil, "closed_at": nil,
		"created_at": updated.Add(-time.Hour).UTC().Format(time.RFC3339), "updated_at": updated.UTC().Format(time.RFC3339),
		"head": map[string]any{"ref": "topic-" + strconv.Itoa(number), "sha": fmt.Sprintf("%040x", number)},
		"user": map[string]any{"login": "contributor", "avatar_url": "https://avatars.example.com/contributor"},
		"base": map[string]any{"repo": map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}}},
	}
}

// mergePull merges pull request number at at.
func (f *fakeIssuesAPI) mergePull(number int, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stamp := at.UTC().Format(time.RFC3339)
	pr := f.pulls[number]
	pr["state"], pr["merged_at"], pr["closed_at"], pr["updated_at"] = "closed", stamp, stamp, stamp
}

// mention records that pull request pr of acme/widgets mentions issue: a
// "cross-referenced" event on the issue's timeline, which shows the pull
// request as it is when the timeline is read.
func (f *fakeIssuesAPI) mention(issue, pr int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeline[issue] = append(f.timeline[issue], timelineRef{pr: pr})
}

// mentionFrom records a mention of issue by a pull request of another
// repository, given as the source GitHub would show.
func (f *fakeIssuesAPI) mentionFrom(issue int, source map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeline[issue] = append(f.timeline[issue], timelineRef{source: source})
}

// timelineRef is one cross-reference on an issue's timeline: a pull request
// of acme/widgets by number, or a fixed source.
type timelineRef struct {
	pr     int
	source map[string]any
}

// serveLinked serves an issue's timeline and single pull requests. Called
// with f.mu held.
func (f *fakeIssuesAPI) serveLinked(w http.ResponseWriter, r *http.Request) bool {
	base := fmt.Sprintf("/repositories/%d/", pollRepoID)
	if strings.HasPrefix(r.URL.Path, base+"pulls") {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, base+"pulls/"))
		pr, ok := f.pulls[n]
		if err != nil || !ok {
			f.t.Errorf("unexpected pull request read %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_ = json.NewEncoder(w).Encode(pr)
		return true
	}
	rest, ok := strings.CutPrefix(r.URL.Path, base+"issues/")
	numberPart, timeline := strings.CutSuffix(rest, "/timeline")
	if !ok || !timeline {
		return false
	}
	number, _ := strconv.Atoi(numberPart)
	if _, found := f.issues[number]; !found {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return true
	}
	// A timeline holds more than cross-references; the poller skips the rest.
	events := []map[string]any{{"event": "labeled", "label": map[string]any{"name": "bug"}}}
	for _, ref := range f.timeline[number] {
		source := ref.source
		if source == nil {
			pr := f.pulls[ref.pr]
			source = map[string]any{
				"number": ref.pr, "title": pr["title"], "body": pr["body"], "state": pr["state"],
				"updated_at":   pr["updated_at"],
				"pull_request": map[string]any{"merged_at": pr["merged_at"]},
				"repository":   map[string]any{"id": pollRepoID, "name": "widgets", "owner": map[string]any{"login": "acme"}},
			}
		}
		events = append(events, map[string]any{"event": "cross-referenced", "source": map[string]any{"type": "issue", "issue": source}})
	}
	q := r.URL.Query()
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page, _ := strconv.Atoi(q.Get("page"))
	if perPage <= 0 || page <= 0 {
		f.t.Errorf("timeline read without paging: %s", r.URL.RawQuery)
		perPage, page = 30, 1
	}
	start := min((page-1)*perPage, len(events))
	end := min(start+perPage, len(events))
	_ = json.NewEncoder(w).Encode(events[start:end])
	return true
}

// linkedRequests counts the timeline and pull request reads since the last
// call, and fails on any read of the pull request list.
func (f *fakeIssuesAPI) linkedRequests(t *testing.T) (timelines, pulls int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests[f.linkedSeen:] {
		path, _, _ := strings.Cut(r, "?")
		switch {
		case strings.HasSuffix(path, "/timeline"):
			timelines++
		case strings.HasPrefix(path, fmt.Sprintf("/repositories/%d/pulls/", pollRepoID)):
			pulls++
		case strings.Contains(path, "/pulls"):
			t.Errorf("pull request list read: %s", r)
		}
	}
	f.linkedSeen = len(f.requests)
	return timelines, pulls
}

// fakePullMirror records what the poller hands to the PR mirror.
type fakePullMirror struct {
	mu        sync.Mutex
	got       []PullRequest
	failUntil map[int32]int // fail a number this many more times
}

func (m *fakePullMirror) MirrorPolledPullRequest(_ context.Context, _ pgtype.UUID, pr PullRequest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failUntil[pr.Number] > 0 {
		m.failUntil[pr.Number]--
		return false, errors.New("mirror unavailable")
	}
	m.got = append(m.got, pr)
	return true, nil
}

// numbers returns the mirrored numbers since the last call.
func (m *fakePullMirror) numbers() []int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int32, len(m.got))
	for i, pr := range m.got {
		out[i] = pr.Number
	}
	m.got = nil
	return out
}

func TestClosingRefs(t *testing.T) {
	cases := []struct {
		text string
		want []issueRef
	}{
		{"Closes #12", []issueRef{{number: 12}}},
		{"fixes: acme/widgets#3 and resolves #4", []issueRef{{owner: "acme", name: "widgets", number: 3}, {number: 4}}},
		{"Resolved\tOther-Org/repo.js#9", []issueRef{{owner: "Other-Org", name: "repo.js", number: 9}}},
		// A bare reference claims nothing: a squash-merge title ends in
		// "(#<pr number>)".
		{"Fix crash (#12)", nil},
		{"see #12", nil},
		{"Fix login #12", nil},
		{"MUL-1: closes MUL-2", nil},
		{"Closes #0", nil},
		{"Closes #99999999999", nil},
	}
	for _, c := range cases {
		if got := closingRefs(c.text); !slices.Equal(got, c.want) {
			t.Errorf("closingRefs(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
}

func TestPullNormalizedKeepsRegisteredRepository(t *testing.T) {
	var g githubPull
	g.Base.Repo.Owner.Login, g.Base.Repo.Name = "Acme", "Widgets"
	if pr := g.normalized(repoName{owner: "acme", name: "widgets"}); pr.Owner != "Acme" || pr.Repo != "Widgets" {
		t.Fatalf("case-only difference: %s/%s, want GitHub's spelling Acme/Widgets", pr.Owner, pr.Repo)
	}
	g.Base.Repo.Owner.Login, g.Base.Repo.Name = "acme", "gadgets"
	if pr := g.normalized(repoName{owner: "acme", name: "widgets"}); pr.Owner != "acme" || pr.Repo != "widgets" {
		t.Fatalf("renamed repository: %s/%s, want the registered acme/widgets", pr.Owner, pr.Repo)
	}
}

// TestPollLinkedPullsMirrorsOnlyTrackedIssues is the acceptance path: of a
// thousand pull requests, the two that close tracked issues are mirrored,
// found through the tracked issues' timelines without reading the pull
// request list. A pull request closing an issue nobody imported, and one that
// only mentions a tracked issue, stay out; an unchanged pull request is not
// read again, and a merge is picked up the next cycle.
func TestPollLinkedPullsMirrorsOnlyTrackedIssues(t *testing.T) {
	env := newPollEnv(t)
	for n := 1; n <= 3; n++ {
		env.api.putIssue(n, "stranger", fmt.Sprintf("Issue %d", n), "", "open", testNow.Add(-time.Hour), nil)
	}
	env.importIssue(t, 1)
	env.importIssue(t, 2)
	t0 := testNow.Add(-30 * time.Minute)
	for n := 1; n <= 1000; n++ {
		env.api.putPull(n, fmt.Sprintf("PR %d", n), "", "open", t0)
	}
	env.api.putPull(7, "Fix crash", "Closes #1", "open", t0)
	env.api.putPull(8, "Fix login", "fixes acme/widgets#2", "open", t0)
	env.api.putPull(9, "Fix untracked", "Closes #3", "open", t0)
	env.api.putPull(10, "Refactor", "see #1", "open", t0)
	env.api.mention(1, 7)
	env.api.mention(1, 10)
	env.api.mention(2, 8)
	env.api.mention(3, 9)

	// Without a mirror no pull request is looked for.
	env.poll(t)
	if timelines, pulls := env.api.linkedRequests(t); timelines != 0 || pulls != 0 {
		t.Fatalf("read %d timelines and %d pull requests without a mirror", timelines, pulls)
	}

	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 || stats.Synced != 2 {
		t.Fatalf("stats = %+v, want 2 pull requests stored", stats)
	}
	got := mirror.numbers()
	slices.Sort(got)
	if !slices.Equal(got, []int32{7, 8}) {
		t.Fatalf("mirrored %v, want only [7 8]", got)
	}
	if timelines, pulls := env.api.linkedRequests(t); timelines != 2 || pulls != 2 {
		t.Fatalf("read %d timelines and %d pull requests, want the 2 tracked issues' and the 2 closing pull requests", timelines, pulls)
	}

	// Nothing changed: the timelines are read, no pull request.
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t)
	if got := mirror.numbers(); len(got) != 0 {
		t.Fatalf("unchanged pull requests mirrored again: %v", got)
	}
	if timelines, pulls := env.api.linkedRequests(t); timelines != 2 || pulls != 0 {
		t.Fatalf("read %d timelines and %d pull requests, want 2 and 0", timelines, pulls)
	}

	env.api.mergePull(7, testNow.Add(2*pollInterval))
	*env.now = testNow.Add(3 * pollInterval)
	env.poll(t)
	mirror.mu.Lock()
	merged := slices.Clone(mirror.got)
	mirror.mu.Unlock()
	if len(merged) != 1 || merged[0].Number != 7 || merged[0].MergedAt == nil {
		t.Fatalf("after the merge mirrored %+v, want #7 merged", merged)
	}
	if _, pulls := env.api.linkedRequests(t); pulls != 1 {
		t.Fatalf("read %d pull requests after the merge, want 1", pulls)
	}
}

// TestPollLinkedPullsKeepsToBudget: timeline pages and pull request reads
// share a budget per repository per cycle, and a scan that runs out continues
// at the issue and page where it stopped.
func TestPollLinkedPullsKeepsToBudget(t *testing.T) {
	env := newPollEnv(t)
	env.svc.pollLimits.perPage, env.svc.pollLimits.pages = 2, 2
	env.api.putIssue(1, "stranger", "One", "", "open", testNow.Add(-time.Hour), nil)
	env.api.putIssue(2, "stranger", "Two", "", "open", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	env.importIssue(t, 2)
	t0 := testNow.Add(-30 * time.Minute)
	// #1's timeline: a label, then PRs 1-3, so two pages of two.
	for n := 1; n <= 3; n++ {
		env.api.putPull(n, fmt.Sprintf("PR %d", n), "Closes #1", "open", t0)
		env.api.mention(1, n)
	}
	env.api.putPull(4, "PR 4", "Closes #2", "open", t0)
	env.api.mention(2, 4)
	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)
	env.api.linkedRequests(t)

	var all []int32
	for cycle := 1; cycle <= 5; cycle++ {
		*env.now = testNow.Add(time.Duration(cycle) * pollInterval)
		if stats := env.poll(t); stats.Failed != 0 {
			t.Fatalf("cycle %d stats = %+v", cycle, stats)
		}
		if timelines, pulls := env.api.linkedRequests(t); timelines+pulls > 2 {
			t.Fatalf("cycle %d sent %d timeline and %d pull request reads, over the budget of 2", cycle, timelines, pulls)
		}
		all = append(all, mirror.numbers()...)
	}
	slices.Sort(all)
	if !slices.Equal(all, []int32{1, 2, 3, 4}) {
		t.Fatalf("mirrored %v over five cycles, want each of 1-4 once", all)
	}
}

// TestPollLinkedPullsRetriesFailedMirror: a pull request the mirror could not
// store is recorded as the repository's error and stored by the next cycle.
func TestPollLinkedPullsRetriesFailedMirror(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	env.api.putPull(7, "Fix crash", "Closes #1", "open", testNow.Add(-time.Minute))
	env.api.mention(1, 7)
	mirror := &fakePullMirror{failUntil: map[int32]int{7: 1}}
	env.svc.SetPullRequestMirror(mirror)

	if stats := env.poll(t); stats.Failed != 1 {
		t.Fatalf("stats = %+v, want the repository failed", stats)
	}
	var lastError string
	env.f.QueryRow(t, `SELECT COALESCE(last_error, '') FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&lastError)
	if !strings.Contains(lastError, "pull request 7") {
		t.Fatalf("last_error = %q", lastError)
	}

	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("retry stats = %+v", stats)
	}
	if got := mirror.numbers(); !slices.Equal(got, []int32{7}) {
		t.Fatalf("retry mirrored %v, want [7]", got)
	}
}

// TestPollLinkedPullsReadsClosedIssueOnce: a closed tracked issue's timeline
// is read once, and again only after the issue changes on GitHub.
func TestPollLinkedPullsReadsClosedIssueOnce(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "closed", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	env.api.putPull(7, "Fix crash", "Closes #1", "closed", testNow.Add(-time.Hour))
	env.api.mention(1, 7)
	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)
	env.api.linkedRequests(t)

	env.poll(t)
	if got := mirror.numbers(); !slices.Equal(got, []int32{7}) {
		t.Fatalf("mirrored %v, want [7]", got)
	}
	*env.now = testNow.Add(pollInterval)
	env.poll(t)
	if timelines, _ := env.api.linkedRequests(t); timelines != 1 {
		t.Fatalf("closed issue's timeline read %d times over two cycles, want once", timelines)
	}

	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(pollInterval+time.Second), nil)
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t)
	if timelines, _ := env.api.linkedRequests(t); timelines != 1 {
		t.Fatalf("issue reopened on GitHub: timeline read %d times, want once", timelines)
	}
}

// TestPollLinkedPullsSkipsUnregisteredRepositories: a pull request of a
// repository the workspace has not registered is neither read nor mirrored,
// however it refers to a tracked issue. One whose repository was renamed
// since it was registered is the same repository id, and is mirrored under
// the registered name.
func TestPollLinkedPullsSkipsUnregisteredRepositories(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	env.api.mentionFrom(1, map[string]any{
		"number": 5, "title": "Fix upstream", "body": "Closes acme/widgets#1", "state": "open",
		"updated_at":   testNow.Add(-time.Minute).UTC().Format(time.RFC3339),
		"pull_request": map[string]any{"merged_at": nil},
		"repository":   map[string]any{"name": "fork", "owner": map[string]any{"login": "stranger"}},
	})
	env.api.putPull(6, "Fix crash", "Closes #1", "open", testNow.Add(-time.Minute))
	env.api.mentionFrom(1, map[string]any{
		"number": 6, "title": "Fix crash", "body": "Closes #1", "state": "open",
		"updated_at":   testNow.Add(-time.Minute).UTC().Format(time.RFC3339),
		"pull_request": map[string]any{"merged_at": nil},
		"repository":   map[string]any{"id": pollRepoID, "name": "gadgets", "owner": map[string]any{"login": "acme"}},
	})
	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)
	env.api.linkedRequests(t)

	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	mirror.mu.Lock()
	got := slices.Clone(mirror.got)
	mirror.mu.Unlock()
	if len(got) != 1 || got[0].Number != 6 || got[0].Owner != "acme" || got[0].Repo != "widgets" {
		t.Fatalf("mirrored %+v, want only the renamed repository's #6 as acme/widgets", got)
	}
	if _, pulls := env.api.linkedRequests(t); pulls != 1 {
		t.Fatalf("read %d pull requests, want 1", pulls)
	}
}

func TestTimelineSourceRepo(t *testing.T) {
	var src timelineSource
	src.Repository.Name, src.Repository.Owner.Login = "Widgets", "Acme"
	if name, ok := src.repo(); !ok || name != (repoName{owner: "Acme", name: "Widgets"}) {
		t.Fatalf("repository object: %v %v", name, ok)
	}
	for url, want := range map[string]bool{
		"https://api.github.com/repos/acme/widgets":       true,
		"https://api.github.com/repos/acme":               false,
		"https://api.github.com/users/acme/widgets":       false,
		"https://api.github.com/repos/acme/widgets/extra": false,
		"": false,
	} {
		src := timelineSource{RepositoryURL: url}
		name, ok := src.repo()
		if ok != want || (ok && name != (repoName{owner: "acme", name: "widgets"})) {
			t.Errorf("repo() of %q = %v %v, want ok %v", url, name, ok, want)
		}
	}
}

// TestPullRequestIssues matches closing references by repository id and
// number, never by the workspace prefix.
func TestPullRequestIssues(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	issue := env.importIssue(t, 1)
	ws := parseTestUUID(t, env.ws)

	cases := []struct {
		owner, repo, title, body string
		want                     bool
	}{
		{"acme", "widgets", "Fix crash", "Closes #1", true},
		{"acme", "widgets", "Fix crash", "fixes ACME/Widgets#1", true},
		{"other", "repo", "Fix crash", "Closes acme/widgets#1", true},
		{"acme", "widgets", "Closes #1 (#7)", "", true},
		// Another repository's #1, an issue not imported, a bare mention.
		{"other", "repo", "Fix crash", "Closes #1", false},
		{"acme", "widgets", "Fix crash", "Closes other/repo#1\nCloses #2", false},
		{"acme", "widgets", "Fix crash (#1)", "see #1", false},
	}
	for _, c := range cases {
		ids, err := env.svc.PullRequestIssues(context.Background(), ws, c.owner, c.repo, c.title, c.body)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		got := len(ids) == 1 && ids[0] == parseTestUUID(t, issue.id)
		if got != c.want || (!c.want && len(ids) != 0) {
			t.Errorf("PullRequestIssues(%s/%s %q %q) = %v, want match %v", c.owner, c.repo, c.title, c.body, ids, c.want)
		}
	}
}

func parseTestUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}
