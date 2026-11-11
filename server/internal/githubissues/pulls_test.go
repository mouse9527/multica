package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
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

// servePulls serves the pulls list the way GitHub documents it: no "since",
// newest updated_at first when asked for sort=updated&direction=desc.
// Called with f.mu held.
func (f *fakeIssuesAPI) servePulls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("state") != "all" || q.Get("sort") != "updated" || q.Get("direction") != "desc" || q.Has("since") {
		f.t.Errorf("pulls listed with %s, want state=all, updated desc, no since", r.URL.RawQuery)
	}
	items := make([]map[string]any, 0, len(f.pulls))
	for _, it := range f.pulls {
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i]["updated_at"].(string), items[j]["updated_at"].(string)
		if a != b {
			return a > b
		}
		return items[i]["number"].(int) > items[j]["number"].(int)
	})
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page, _ := strconv.Atoi(q.Get("page"))
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	_ = json.NewEncoder(w).Encode(items[start:end])
}

func (f *fakeIssuesAPI) pullRequests() int {
	n := 0
	for _, r := range f.seen() {
		if strings.HasPrefix(r, fmt.Sprintf("/repositories/%d/pulls?", pollRepoID)) {
			n++
		}
	}
	return n
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

func (env *pollEnv) pullsCursor(t *testing.T) (time.Time, bool) {
	t.Helper()
	var at pgtype.Timestamptz
	env.f.QueryRow(t, `SELECT pulls_cursor FROM github_repo_sync WHERE workspace_id = $1 AND repo_id = $2`,
		env.ws, pollRepoID).Scan(&at)
	return at.Time, at.Valid
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

// TestPollPullsReadsDownToCursor: the first read takes the whole list, later
// ones stop at the first pull request older than the cursor's overlap window,
// and nothing is read without a mirror.
func TestPollPullsReadsDownToCursor(t *testing.T) {
	env := newPollEnv(t)
	env.svc.pollLimits.perPage = 2
	t0 := testNow.Add(-5 * time.Hour)
	for n := 1; n <= 5; n++ {
		env.api.putPull(n, fmt.Sprintf("PR %d", n), "", "open", t0.Add(time.Duration(n)*time.Hour))
	}

	env.poll(t)
	if n := env.api.pullRequests(); n != 0 {
		t.Fatalf("pulls read %d times without a mirror", n)
	}

	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 || stats.Synced != 5 {
		t.Fatalf("first read stats = %+v", stats)
	}
	if got := mirror.numbers(); !slices.Equal(got, []int32{5, 4, 3, 2, 1}) {
		t.Fatalf("first read mirrored %v, want newest first 5..1", got)
	}
	if n := env.api.pullRequests(); n != 3 {
		t.Fatalf("first read took %d requests, want 3 pages", n)
	}
	cursor, ok := env.pullsCursor(t)
	if want := t0.Add(5 * time.Hour); !ok || !cursor.Equal(want) {
		t.Fatalf("pulls cursor = %v %v, want %v", cursor, ok, want)
	}

	// PR 6 opens; PR 5 is inside the overlap window and read again, PR 4 is
	// older and ends the read.
	env.api.putPull(6, "PR 6", "Closes #1", "open", testNow.Add(30*time.Second))
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t)
	if got := mirror.numbers(); !slices.Equal(got, []int32{6, 5}) {
		t.Fatalf("second read mirrored %v, want [6 5]", got)
	}
	if n := env.api.pullRequests(); n != 5 {
		t.Fatalf("pulls requests = %d, want 5 (2 for the second read)", n)
	}
	if cursor, _ := env.pullsCursor(t); !cursor.Equal(testNow.Add(30 * time.Second)) {
		t.Fatalf("pulls cursor = %v, want PR 6's updated_at", cursor)
	}
}

// TestPollPullsResumesAcrossCycles: a first read longer than a cycle's page
// budget continues where it stopped, the cursor moves only once the read is
// complete, and a pull request updated during the read is not lost.
func TestPollPullsResumesAcrossCycles(t *testing.T) {
	env := newPollEnv(t)
	env.svc.pollLimits.perPage, env.svc.pollLimits.pages = 2, 1
	mirror := &fakePullMirror{}
	env.svc.SetPullRequestMirror(mirror)
	t0 := testNow.Add(-5 * time.Hour)
	for n := 1; n <= 5; n++ {
		env.api.putPull(n, fmt.Sprintf("PR %d", n), "", "open", t0.Add(time.Duration(n)*10*time.Minute))
	}

	env.poll(t)
	if got := mirror.numbers(); !slices.Equal(got, []int32{5, 4}) {
		t.Fatalf("cycle 1 mirrored %v", got)
	}
	if _, ok := env.pullsCursor(t); ok {
		t.Fatal("cursor moved before the first read was complete")
	}
	// PR 2, not read yet, is updated: it moves to the front of the list.
	env.api.putPull(2, "PR 2 edited", "", "open", testNow.Add(-time.Minute))
	for cycle, want := range [][]int32{{4, 3}, {1}} {
		*env.now = testNow.Add(time.Duration(cycle+1) * pollInterval)
		env.poll(t)
		if got := mirror.numbers(); !slices.Equal(got, want) {
			t.Fatalf("cycle %d mirrored %v, want %v", cycle+2, got, want)
		}
	}
	cursor, ok := env.pullsCursor(t)
	if want := t0.Add(50 * time.Minute); !ok || !cursor.Equal(want) {
		t.Fatalf("cursor after the first read = %v %v, want PR 5's %v", cursor, ok, want)
	}

	// The next read starts from the front and picks up PR 2.
	env.svc.pollLimits.pages = 10
	*env.now = testNow.Add(4 * pollInterval)
	env.poll(t)
	if got := mirror.numbers(); !slices.Equal(got, []int32{2, 5}) {
		t.Fatalf("read after the backfill mirrored %v, want [2 5]", got)
	}
}

// TestPollPullsRetriesFailedMirror: a pull request the mirror could not store
// stops the read without moving the cursor, is recorded as the repository's
// error, and is stored by the next cycle.
func TestPollPullsRetriesFailedMirror(t *testing.T) {
	env := newPollEnv(t)
	mirror := &fakePullMirror{failUntil: map[int32]int{2: 1}}
	env.svc.SetPullRequestMirror(mirror)
	t0 := testNow.Add(-time.Hour)
	for n := 1; n <= 3; n++ {
		env.api.putPull(n, fmt.Sprintf("PR %d", n), "", "open", t0.Add(time.Duration(n)*time.Minute))
	}

	if stats := env.poll(t); stats.Failed != 1 {
		t.Fatalf("stats = %+v, want the repository failed", stats)
	}
	if got := mirror.numbers(); !slices.Equal(got, []int32{3}) {
		t.Fatalf("mirrored %v before the failure, want [3]", got)
	}
	if _, ok := env.pullsCursor(t); ok {
		t.Fatal("cursor moved past a pull request that was not stored")
	}
	var lastError string
	env.f.QueryRow(t, `SELECT COALESCE(last_error, '') FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&lastError)
	if !strings.Contains(lastError, "pull request 2") {
		t.Fatalf("last_error = %q", lastError)
	}

	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("retry stats = %+v", stats)
	}
	if got := mirror.numbers(); !slices.Equal(got, []int32{3, 2, 1}) {
		t.Fatalf("retry mirrored %v, want [3 2 1]", got)
	}
	if cursor, ok := env.pullsCursor(t); !ok || !cursor.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("cursor = %v %v", cursor, ok)
	}
}

// TestPullRequestIssues matches closing references by repository id and
// number, never by the workspace prefix.
func TestPullRequestIssues(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.poll(t)
	issue := env.issue(t, 1)
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
