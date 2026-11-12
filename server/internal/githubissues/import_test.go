package githubissues

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (env *pollEnv) importRef(t *testing.T, ref IssueRef) (string, bool, error) {
	t.Helper()
	ws, err := util.ParseUUID(env.ws)
	if err != nil {
		t.Fatal(err)
	}
	id, created, err := env.svc.ImportIssue(context.Background(), ws, ref)
	return util.UUIDToString(id), created, err
}

// listRequests are the issues and comments list requests sent so far, with
// their "since".
func (env *pollEnv) listRequests(t *testing.T) []url.Values {
	t.Helper()
	var out []url.Values
	for _, r := range env.api.seen() {
		path, query, _ := strings.Cut(r, "?")
		if path == fmt.Sprintf("/repositories/%d/issues", pollRepoID) || path == fmt.Sprintf("/repositories/%d/issues/comments", pollRepoID) {
			q, err := url.ParseQuery(query)
			if err != nil {
				t.Fatal(err)
			}
			q.Set("path", path)
			out = append(out, q)
		}
	}
	return out
}

// TestPollLeavesIssuesNobodyImported is the acceptance path of on-request
// import: a repository with 1000 issues nobody imported, all of them
// updated, brings none of them in over two cycles. Importing one brings it
// and its comments; from then on its edits and comments are followed, and the
// other 999 stay out however they change.
func TestPollLeavesIssuesNobodyImported(t *testing.T) {
	env := newPollEnv(t)
	const total = 1000
	old := testNow.Add(-24 * time.Hour)
	for n := 1; n <= total; n++ {
		env.api.putIssue(n, "stranger", "Issue "+strconv.Itoa(n), "", "open", old, nil)
		env.api.putComment(int64(10_000+n), n, "stranger", "Comment on "+strconv.Itoa(n), old)
	}
	env.poll(t)
	// Every one of them is updated on GitHub after that, one a second. With
	// nothing tracked the repository is not followed at all (#57).
	for n := 1; n <= total; n++ {
		at := old.Add(time.Duration(n) * time.Second)
		env.api.putIssue(n, "stranger", "Issue "+strconv.Itoa(n)+" (edited)", "", "open", at, nil)
		env.api.putComment(int64(20_000+n), n, "stranger", "New on "+strconv.Itoa(n), at)
	}
	for cycle := 1; cycle <= 2; cycle++ {
		*env.now = testNow.Add(time.Duration(cycle) * pollInterval)
		if stats := env.poll(t); stats.Failed != 0 || stats.Synced != 0 {
			t.Fatalf("cycle %d stats = %+v, want nothing synced", cycle, stats)
		}
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 0 {
		t.Fatalf("issues after two cycles = %d, want 0", n)
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE workspace_id = $1`, env.ws); n != 0 {
		t.Fatalf("comments after two cycles = %d, want 0", n)
	}
	if seen := env.api.seen(); len(seen) != 0 {
		t.Fatalf("cycles with nothing tracked sent %v", seen)
	}

	const picked = 7
	before := len(env.api.seen())
	id, created, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: picked})
	if err != nil || !created {
		t.Fatalf("import = %s %v %v", id, created, err)
	}
	// The repository's id (never resolved: nothing polled it), then one issue
	// and its comments: nothing about the other issues is read.
	for _, r := range env.api.seen()[before:] {
		if path, _, _ := strings.Cut(r, "?"); path != "/repos/acme/widgets" && path != fmt.Sprintf("/repositories/%d/issues/%d", pollRepoID, picked) &&
			path != fmt.Sprintf("/repositories/%d/issues/%d/comments", pollRepoID, picked) {
			t.Fatalf("import sent %s", r)
		}
	}
	if got := env.issue(t, picked); got.id != id || got.title != "Issue 7 (edited)" {
		t.Fatalf("imported issue = %+v", got)
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, id); n != 2 {
		t.Fatalf("comments of the imported issue = %d, want 2", n)
	}

	// Later edits of the imported issue are followed; the others still not.
	later := testNow.Add((total + 60) * time.Second)
	for _, n := range []int{picked, picked + 1} {
		env.api.putIssue(n, "stranger", "Issue "+strconv.Itoa(n)+" (again)", "", "open", later, nil)
		env.api.putComment(int64(30_000+n), n, "stranger", "Again on "+strconv.Itoa(n), later)
	}
	*env.now = testNow.Add(3 * pollInterval)
	if stats := env.poll(t); stats.Failed != 0 || stats.Synced != 2 {
		t.Fatalf("after the import stats = %+v, want the issue and its comment", stats)
	}
	if got := env.issue(t, picked); got.title != "Issue 7 (again)" {
		t.Fatalf("imported issue after the edit = %+v", got)
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 1 {
		t.Fatalf("issues = %d, want only the imported one", n)
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE workspace_id = $1`, env.ws); n != 3 {
		t.Fatalf("comments = %d, want the 3 on the imported issue", n)
	}
	// No list request ever went without "since": the history is never read.
	for _, q := range env.listRequests(t) {
		if q.Get("since") == "" {
			t.Fatalf("list request without since: %v", q)
		}
	}
}

// TestPollFirstLookDoesNotBackfill: a repository polled for the first time is
// followed from that moment, and a repository with cursors keeps reading from
// them. A cursor that is set is never moved by starting the other one.
func TestPollFirstLookDoesNotBackfill(t *testing.T) {
	env := newPollEnv(t)
	old := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Old", "", "open", old, nil)
	env.api.putComment(501, 1, "stranger", "Old comment", old)
	env.trackWithoutCursors(t, 2)

	if stats := env.poll(t); stats.Failed != 0 || stats.Synced != 0 {
		t.Fatalf("first poll stats = %+v", stats)
	}
	window := testNow.Add(-pollOverlap).Format(time.RFC3339)
	requests := env.listRequests(t)
	if len(requests) != 2 {
		t.Fatalf("first poll list requests = %v, want one per stream", requests)
	}
	for _, q := range requests {
		if q.Get("since") != window {
			t.Fatalf("first poll read %s since %q, want %q", q.Get("path"), q.Get("since"), window)
		}
	}
	var issues, comments time.Time
	env.f.QueryRow(t, `SELECT issues_cursor, comments_cursor FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&issues, &comments)
	if !issues.Equal(testNow) || !comments.Equal(testNow) {
		t.Fatalf("cursors = %v %v, want both started at %v", issues, comments, testNow)
	}

	// A repository with cursors reads from them, as before.
	env.rewindCursors(t, old)
	*env.now = testNow.Add(pollInterval)
	seen := len(env.listRequests(t))
	env.poll(t)
	for _, q := range env.listRequests(t)[seen:] {
		if want := old.Add(-pollOverlap).Format(time.RFC3339); q.Get("since") != want {
			t.Fatalf("poll with cursors read %s since %q, want %q", q.Get("path"), q.Get("since"), want)
		}
	}

	// Only an empty cursor is started.
	env.f.Exec(t, `UPDATE github_repo_sync SET issues_cursor = $2, comments_cursor = NULL WHERE workspace_id = $1`, env.ws, old)
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t)
	env.f.QueryRow(t, `SELECT issues_cursor, comments_cursor FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&issues, &comments)
	if !issues.Equal(old) || !comments.Equal(*env.now) {
		t.Fatalf("cursors = %v %v, want the issues cursor kept at %v and comments started at %v", issues, comments, old, *env.now)
	}
}

// TestImportIssue covers what an import stores, that importing again stores
// and sends nothing, and the refusals that send nothing or store nothing.
func TestImportIssue(t *testing.T) {
	env := newPollEnv(t)
	// main.go turns GitHub identifiers on together with the feature.
	previous := issuekey.Enabled()
	issuekey.SetEnabled(true)
	t.Cleanup(func() { issuekey.SetEnabled(previous) })
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash on start", "It crashes.", "open", t0, nil)
	env.api.putComment(501, 1, "stranger", "Same here", t0.Add(time.Minute))
	env.api.putIssue(2, "stranger", "Fix crash", "PR body", "open", t0,
		map[string]any{"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widgets/pulls/2"}})

	id, created, err := env.importRef(t, IssueRef{Owner: "ACME", Repo: "Widgets", Number: 1})
	if err != nil || !created {
		t.Fatalf("import = %s %v %v", id, created, err)
	}
	got := env.issue(t, 1)
	if got.id != id || got.title != "Crash on start" || got.status != "backlog" || got.state != "open" {
		t.Fatalf("imported issue = %+v", got)
	}
	if got.creatorType != "member" || got.creatorID != env.owner || got.projectID != env.project {
		t.Fatalf("imported issue creator/project = %+v, want owner %s in project %s", got, env.owner, env.project)
	}
	if got.description != ExternalIssueDescription("stranger", "It crashes.") {
		t.Fatalf("description = %q", got.description)
	}
	var author, content string
	env.f.QueryRow(t, `SELECT c.author_type, c.content FROM comment c JOIN comment_external x ON x.comment_id = c.id
		WHERE x.workspace_id = $1 AND x.external_id = 501`, env.ws).Scan(&author, &content)
	if author != externalAuthorType || content != ExternalCommentContent("stranger", "Same here") {
		t.Fatalf("imported comment = %s %q", author, content)
	}
	// The issue:created payload has no description, so the subscriber
	// listener cannot read mention links out of GitHub text.
	env.mu.Lock()
	var createdEvent map[string]any
	for _, e := range env.events {
		if e.Type == protocol.EventIssueCreated {
			createdEvent = e.Payload.(map[string]any)["issue"].(map[string]any)
		}
	}
	env.mu.Unlock()
	if createdEvent == nil || createdEvent["description"] != nil || createdEvent["identifier"] != "acme/widgets#1" || createdEvent["provider"] != "github" {
		t.Fatalf("issue:created payload = %v", createdEvent)
	}

	// Importing again returns the same issue, stores nothing new and reads
	// only its comments again.
	sent := len(env.api.seen())
	again, created, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 1})
	if err != nil || created || again != id {
		t.Fatalf("second import = %s %v %v, want %s not created", again, created, err, id)
	}
	for _, r := range env.api.seen()[sent:] {
		if path, _, _ := strings.Cut(r, "?"); path != fmt.Sprintf("/repositories/%d/issues/1/comments", pollRepoID) {
			t.Fatalf("second import sent %s", r)
		}
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, id); n != 1 {
		t.Fatalf("comments after the second import = %d, want 1", n)
	}

	// A pull request number is refused and nothing is stored.
	if _, _, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 2}); !errors.Is(err, ErrPullRequest) {
		t.Fatalf("pull request import: %v", err)
	}
	// A repository outside the allowlist is refused before any request.
	sent = len(env.api.seen())
	if _, _, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "secrets", Number: 1}); !errors.Is(err, ErrRepoNotRegistered) {
		t.Fatalf("unregistered import: %v", err)
	}
	if got := env.api.seen()[sent:]; len(got) != 0 {
		t.Fatalf("unregistered import sent %v", got)
	}
	// An issue GitHub does not have.
	if _, _, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 99}); !errors.Is(err, ErrGitHubIssueNotFound) {
		t.Fatalf("missing issue import: %v", err)
	}
	// No sync account: refused, nothing stored.
	env.api.putIssue(3, "stranger", "Docs typo", "", "open", t0, nil)
	env.f.Exec(t, `UPDATE member SET role = 'member' WHERE workspace_id = $1`, env.ws)
	if _, _, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 3}); !errors.Is(err, ErrSyncAccountUnavailable) {
		t.Fatalf("import without a sync account: %v", err)
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 1 {
		t.Fatalf("issues = %d, want only the first import", n)
	}
	// While GitHub has polling paused, nothing is sent.
	env.f.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1`, env.ws)
	env.svc.pollGate.pause(testNow.Add(time.Minute))
	sent = len(env.api.seen())
	if _, _, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 3}); !errors.Is(err, ErrImportPaused) {
		t.Fatalf("import while paused: %v", err)
	}
	if got := env.api.seen()[sent:]; len(got) != 0 {
		t.Fatalf("import while paused sent %v", got)
	}
}

// TestImportReadsEveryComment: an import reads every comment page, past the
// pages an on-view refresh reads, since the poller never reads comments older
// than the repository's cursor. An import cut short midway keeps the issue,
// says it is incomplete, and importing again finishes it without a second
// issue; a rate limit midway also pauses polling.
func TestImportReadsEveryComment(t *testing.T) {
	env := newPollEnv(t)
	env.svc.pollLimits.perPage = 10
	const comments = 10*refreshCommentPages + 5 // past the refresh's pages
	t0 := testNow.Add(-24 * time.Hour)
	for _, n := range []int{1, 2} {
		env.api.putIssue(n, "stranger", "Issue "+strconv.Itoa(n), "", "open", t0, nil)
		for i := 1; i <= comments; i++ {
			env.api.putComment(int64(n*10_000+i), n, "stranger", "Comment "+strconv.Itoa(i), t0)
		}
	}
	countComments := func(id string) int {
		return env.count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, id)
	}

	id, created, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 1})
	if err != nil || !created {
		t.Fatalf("import = %s %v %v", id, created, err)
	}
	if n := countComments(id); n != comments {
		t.Fatalf("comments after the import = %d, want all %d", n, comments)
	}

	// GitHub fails on page 3: the issue is kept, the import is incomplete.
	env.api.mu.Lock()
	env.api.commentsFault.page, env.api.commentsFault.status = 3, http.StatusBadGateway
	env.api.mu.Unlock()
	id, created, err = env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 2})
	if !errors.Is(err, ErrImportIncomplete) || !created || id == "" {
		t.Fatalf("interrupted import = %q %v %v, want the new issue and ErrImportIncomplete", id, created, err)
	}
	if n := countComments(id); n != 20 {
		t.Fatalf("comments after the interrupted import = %d, want the 2 pages before the failure", n)
	}
	env.api.mu.Lock()
	env.api.commentsFault.page = 0
	env.api.mu.Unlock()
	again, created, err := env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 2})
	if err != nil || created || again != id {
		t.Fatalf("finishing import = %s %v %v, want %s not created", again, created, err, id)
	}
	if n := countComments(id); n != comments {
		t.Fatalf("comments after finishing the import = %d, want all %d", n, comments)
	}

	// Throttled midway: incomplete, the rate limit still visible, polling
	// paused for the wait GitHub asked.
	env.api.mu.Lock()
	env.api.commentsFault.page, env.api.commentsFault.status = 2, http.StatusTooManyRequests
	env.api.mu.Unlock()
	_, _, err = env.importRef(t, IssueRef{Owner: "acme", Repo: "widgets", Number: 2})
	var limited *ghsnapshot.RateLimitError
	if !errors.Is(err, ErrImportIncomplete) || !errors.As(err, &limited) {
		t.Fatalf("throttled import: %v", err)
	}
	if !env.svc.pollGate.paused(testNow.Add(30 * time.Second)) {
		t.Fatal("polling not paused after a throttled import")
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 2 {
		t.Fatalf("issues = %d, want 2", n)
	}
}

func TestParseIssueRef(t *testing.T) {
	ok := []struct {
		in   string
		want IssueRef
	}{
		{"acme/widgets#7", IssueRef{"acme", "widgets", 7}},
		{"  acme/my.repo_x#12  ", IssueRef{"acme", "my.repo_x", 12}},
		{"https://github.com/acme/widgets/issues/7", IssueRef{"acme", "widgets", 7}},
		{"https://www.GitHub.com/acme/widgets/issues/7/", IssueRef{"acme", "widgets", 7}},
		{"https://github.com/acme/widgets/issues/7#issuecomment-1", IssueRef{"acme", "widgets", 7}},
		{"https://github.com/acme/widgets/issues/7?q=1", IssueRef{"acme", "widgets", 7}},
	}
	for _, c := range ok {
		got, err := ParseIssueRef(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseIssueRef(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"https://github.com/acme/widgets/pull/7", "https://github.com/acme/widgets/pulls/7"} {
		if _, err := ParseIssueRef(in); !errors.Is(err, ErrPullRequest) {
			t.Errorf("ParseIssueRef(%q) = %v, want ErrPullRequest", in, err)
		}
	}
	for _, in := range []string{
		"", "acme/widgets", "acme/widgets#0", "acme/widgets#-1", "#7", "WID-7",
		"https://gitlab.com/acme/widgets/issues/7", "https://github.com:8443/acme/widgets/issues/7",
		"https://user@github.com/acme/widgets/issues/7", "ftp://github.com/acme/widgets/issues/7",
		"https://github.com/acme/widgets/issues/x", "https://github.com/acme/widgets/discussions/7",
		"https://github.com/acme/widgets/issues/7/comments", "https://github.com/acme/../issues/7",
		"https://github.com/acme/widgets/issues/99999999999",
	} {
		if _, err := ParseIssueRef(in); !errors.Is(err, ErrInvalidIssueRef) {
			t.Errorf("ParseIssueRef(%q) = %v, want ErrInvalidIssueRef", in, err)
		}
	}
}

// trackWithoutCursors makes acme/widgets a followed repository whose cursors
// are still empty: it is pinned and tracks issue number (opened from Multica,
// say, which starts no cursor), and nothing has polled it yet. Only followed
// repositories are polled (#57).
func (env *pollEnv) trackWithoutCursors(t *testing.T, number int) {
	t.Helper()
	if _, err := env.svc.RepoClient(context.Background(), util.MustParseUUID(env.ws), "acme", "widgets"); err != nil {
		t.Fatalf("pin acme/widgets: %v", err)
	}
	issue := env.f.Issue(t, "tracked", testutil.Cols{"project_id": env.project})
	env.f.Exec(t, `INSERT INTO issue_external (issue_id, workspace_id, provider, external_repo_id, external_repo,
		external_number, external_node_id, state, external_updated_at)
		VALUES ($1, $2, 'github', $3, 'acme/widgets', $4, 'I_tracked', 'open', $5)`,
		issue, env.ws, pollRepoID, number, testNow.Add(-2*time.Hour))
}
