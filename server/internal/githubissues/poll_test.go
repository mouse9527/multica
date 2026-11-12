package githubissues

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const pollRepoID = 42

// fakeIssuesAPI serves one repository's issues and comments lists the way
// GitHub documents them: ascending by updated_at, "since" inclusive, paged
// by per_page and page. Items are replaced by id, so a test edits an issue
// or comment by putting a new snapshot.
type fakeIssuesAPI struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	issues   map[int]map[string]any // by number
	comments map[int64]map[string]any
	pulls    map[int]map[string]any // by number
	// timeline is each issue's cross-references, by issue number.
	timeline map[int][]timelineRef
	// linkedSeen is how many requests linkedRequests has counted.
	linkedSeen int
	requests   []string
	// throttle, when set, answers every list request with 429.
	throttle string
	// commentsFault, when set, answers that page of an issue's comment
	// list with that status.
	commentsFault struct{ page, status int }
	// writes is the single-item API of write_fake_test.go.
	writes fakeWrites
	// issueReplies answers the read of an issue with a status instead of
	// the issue: deleted (404, 410) or transferred (301 and a Location).
	issueReplies map[int]issueReply
	// extra, when set, answers the requests it reports it answered, before
	// the routes of this repository (another repository). Called with mu held.
	extra func(w http.ResponseWriter, r *http.Request) bool
}

type issueReply struct {
	status   int
	location string
}

func newFakeIssuesAPI(t *testing.T) *fakeIssuesAPI {
	f := &fakeIssuesAPI{t: t, issues: map[int]map[string]any{}, comments: map[int64]map[string]any{}, pulls: map[int]map[string]any{}, timeline: map[int][]timelineRef{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIssuesAPI) putIssue(number int, login, title, body, state string, updated time.Time, extra map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := map[string]any{
		"id": 1000 + number, "node_id": "I_" + strconv.Itoa(number), "number": number,
		"title": title, "body": body, "state": state, "state_reason": nil,
		"updated_at": updated.UTC().Format(time.RFC3339), "user": map[string]any{"login": login},
	}
	for k, v := range extra {
		item[k] = v
	}
	f.issues[number] = item
}

func (f *fakeIssuesAPI) putComment(id int64, number int, login, body string, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[id] = map[string]any{
		"id": id, "body": body, "updated_at": updated.UTC().Format(time.RFC3339),
		"issue_url": fmt.Sprintf("https://api.github.com/repos/acme/widgets/issues/%d", number),
		"user":      map[string]any{"login": login, "avatar_url": "https://avatars.example.com/" + login},
	}
}

func (f *fakeIssuesAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeIssuesAPI) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+fakeToken {
		f.t.Errorf("Authorization = %q", got)
	}
	if f.serveItem(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/repos/acme/widgets" {
		_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(pollRepoID) + `}`))
		return
	}
	if f.throttle != "" {
		w.Header().Set("Retry-After", f.throttle)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
		return
	}
	if f.extra != nil && f.extra(w, r) {
		return
	}
	if f.serveLinked(w, r) || f.serveIssue(w, r) {
		return
	}
	var items []map[string]any
	switch r.URL.Path {
	case fmt.Sprintf("/repositories/%d/issues", pollRepoID):
		if r.URL.Query().Get("state") != "all" {
			f.t.Errorf("issues listed without state=all: %s", r.URL.RawQuery)
		}
		for _, it := range f.issues {
			items = append(items, it)
		}
	case fmt.Sprintf("/repositories/%d/issues/comments", pollRepoID):
		for _, it := range f.comments {
			items = append(items, it)
		}
	default:
		f.t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if q.Get("sort") != "updated" || q.Get("direction") != "asc" {
		f.t.Errorf("list not sorted by updated asc: %s", r.URL.RawQuery)
	}
	if since := q.Get("since"); since != "" {
		at, err := time.Parse(time.RFC3339, since)
		if err != nil {
			f.t.Errorf("bad since %q", since)
		}
		kept := items[:0]
		for _, it := range items {
			if updated, _ := time.Parse(time.RFC3339, it["updated_at"].(string)); !updated.Before(at) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i]["updated_at"].(string), items[j]["updated_at"].(string)
		if a != b {
			return a < b
		}
		return fmt.Sprint(items[i]["id"]) < fmt.Sprint(items[j]["id"])
	})
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page, _ := strconv.Atoi(q.Get("page"))
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	_ = json.NewEncoder(w).Encode(items[start:end])
}

type pollEnv struct {
	f       *testutil.Fixture
	svc     *Service
	api     *fakeIssuesAPI
	now     *time.Time
	ws      string
	owner   string
	project string

	mu     sync.Mutex
	events []events.Event
}

// newPollEnv builds the poll environment with the poll and the on-view
// refresh switched on, as before they became switches.
func newPollEnv(t *testing.T) *pollEnv {
	t.Helper()
	return newPollEnvWith(t, map[string]string{EnvPollEnabled: "true", EnvRefreshOnView: "true"})
}

// newPollEnvWith builds the poll environment with extra environment
// variables on top of the feature's own.
func newPollEnvWith(t *testing.T, extraEnv map[string]string) *pollEnv {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	// The poller reads registered repositories across workspaces, so it gets
	// a database of its own.
	pool := testutil.IsolatedDB(t, dsn)
	f := testutil.New(pool, "", "")
	suffix := uuid.NewString()[:8]
	env := &pollEnv{f: f, api: newFakeIssuesAPI(t)}
	env.owner = f.User(t, "Owner", "poll-owner-"+suffix+"@example.com")
	env.ws = f.Workspace(t, "Poll", "poll-"+suffix)
	f.WorkspaceID, f.UserID = env.ws, env.owner
	f.Member(t, env.ws, env.owner, "owner")
	env.project = f.Project(t, "Widgets")
	f.Insert(t, "project_resource", testutil.Cols{
		"project_id": env.project, "workspace_id": env.ws,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/widgets"}`,
	})

	now := testNow
	env.now = &now
	bus := events.New()
	for _, eventType := range []string{protocol.EventIssueCreated, protocol.EventIssueUpdated, protocol.EventCommentCreated} {
		bus.Subscribe(eventType, func(e events.Event) {
			env.mu.Lock()
			env.events = append(env.events, e)
			env.mu.Unlock()
		})
	}
	vars := map[string]string{EnvEnabled: "true", EnvToken: fakeToken, EnvTokenExpiresAt: "never"}
	for k, v := range extraEnv {
		vars[k] = v
	}
	cfg := LoadConfig(envOf(vars))
	env.svc = New(cfg, Options{
		DB: pool, Bus: bus, APIBase: env.api.server.URL,
		Now: func() time.Time { return *env.now }, Logger: slog.New(slog.DiscardHandler),
	})
	env.svc.state.markAccepted("octocat", testNow)
	return env
}

func (env *pollEnv) poll(t *testing.T) PollStats {
	t.Helper()
	stats, err := env.svc.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return stats
}

func (env *pollEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	return env.f.Count(t, sql, args...)
}

// importIssue imports GitHub issue number the way a person does
// (ImportIssue) and returns it.
func (env *pollEnv) importIssue(t *testing.T, number int) polledIssue {
	t.Helper()
	ws, err := util.ParseUUID(env.ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.svc.ImportIssue(context.Background(), ws, IssueRef{Owner: "acme", Repo: "widgets", Number: int32(number)}, seeAllProjects); err != nil {
		t.Fatalf("import #%d: %v", number, err)
	}
	return env.issue(t, number)
}

// rewindCursors puts the issues and comments cursors back to at, as for a
// repository followed since then: the next poll reads what GitHub changed
// after at, less the overlap.
func (env *pollEnv) rewindCursors(t *testing.T, at time.Time) {
	t.Helper()
	env.f.Exec(t, `UPDATE github_repo_sync SET issues_cursor = $2, comments_cursor = $2 WHERE workspace_id = $1`, env.ws, at)
}

type polledIssue struct {
	id, title, description, status, creatorType, creatorID, projectID, state string
}

func (env *pollEnv) issue(t *testing.T, number int) polledIssue {
	t.Helper()
	var got polledIssue
	env.f.QueryRow(t, `
		SELECT i.id::text, i.title, COALESCE(i.description, ''), i.status, i.creator_type, i.creator_id::text,
		       COALESCE(i.project_id::text, ''), x.state
		FROM issue_external x JOIN issue i ON i.id = x.issue_id
		WHERE x.workspace_id = $1 AND x.external_repo_id = $2 AND x.external_number = $3`,
		env.ws, pollRepoID, number).Scan(&got.id, &got.title, &got.description, &got.status,
		&got.creatorType, &got.creatorID, &got.projectID, &got.state)
	return got
}

// TestPollFollowsImportedIssuesWithinTwoCycles is the acceptance path: an
// imported issue edited and commented on by someone else on GitHub is up to
// date in Multica after the cycle that follows; issues nobody imported, pull
// requests and the comments on them stay out, and polling the same data
// again writes no second row.
func TestPollFollowsImportedIssuesWithinTwoCycles(t *testing.T) {
	env := newPollEnv(t)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash on start", "It crashes.", "open", t0, nil)
	env.api.putComment(501, 1, "stranger", "Same here", t0.Add(time.Minute))
	env.importIssue(t, 1)

	// Before the first cycle someone edits and comments on the imported
	// issue, opens issue 3 and comments on it, and opens a pull request.
	env.api.putIssue(1, "stranger", "Crash on start (macOS)", "It crashes.", "open", testNow.Add(-40*time.Second), nil)
	env.api.putComment(502, 1, "stranger", "Me too", testNow.Add(-35*time.Second))
	env.api.putIssue(2, "stranger", "Fix crash", "PR body", "open", testNow.Add(-30*time.Second),
		map[string]any{"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widgets/pulls/2"}})
	env.api.putComment(503, 2, "reviewer", "LGTM", testNow.Add(-25*time.Second))
	env.api.putIssue(3, "stranger", "Docs typo", "", "open", testNow.Add(-20*time.Second), nil)
	env.api.putComment(504, 3, "stranger", "Line 4", testNow.Add(-15*time.Second))

	stats := env.poll(t)
	if stats.Repos != 1 || stats.Failed != 0 || stats.Synced != 2 {
		t.Fatalf("first cycle stats = %+v", stats)
	}
	if got := env.issue(t, 1); got.title != "Crash on start (macOS)" {
		t.Fatalf("imported issue after the cycle = %+v", got)
	}
	var author, content string
	env.f.QueryRow(t, `SELECT c.author_type, c.content FROM comment c JOIN comment_external x ON x.comment_id = c.id
		WHERE x.workspace_id = $1 AND x.external_id = 502`, env.ws).Scan(&author, &content)
	if author != externalAuthorType || content != ExternalCommentContent("stranger", "Me too") {
		t.Fatalf("new comment = %s %q", author, content)
	}
	if n := env.count(t, `SELECT count(*) FROM issue_external WHERE workspace_id = $1`, env.ws); n != 1 {
		t.Fatalf("issue_external rows = %d, want 1 (issue 3 and the pull request stay out)", n)
	}
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE workspace_id = $1`, env.ws); n != 2 {
		t.Fatalf("comment_external rows = %d, want 2 (comments on issue 3 and the pull request stay out)", n)
	}

	// The second cycle brings the next comment.
	env.api.putComment(505, 1, "stranger", "Still there", testNow.Add(30*time.Second))
	env.api.putIssue(3, "stranger", "Docs typo", "Line 4 of the guide", "open", testNow.Add(40*time.Second), nil)
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Synced != 1 || stats.Failed != 0 {
		t.Fatalf("second cycle stats = %+v", stats)
	}
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE workspace_id = $1 AND external_id = 505`, env.ws); n != 1 {
		t.Fatal("second comment not synced")
	}

	// Re-reading everything (the overlap, or a cursor set back) adds nothing.
	env.rewindCursors(t, t0.Add(-time.Hour))
	if stats := env.poll(t); stats.Synced != 0 || stats.Failed != 0 {
		t.Fatalf("re-poll stats = %+v, want nothing synced", stats)
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 1 {
		t.Fatalf("issues = %d, want 1", n)
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE workspace_id = $1`, env.ws); n != 3 {
		t.Fatalf("comments = %d, want 3", n)
	}
}

// TestPollOrdersSnapshotsByExternalUpdatedAt replays an older snapshot of an
// issue and of a comment inside the overlap window: neither rolls anything
// back, and a newer one is applied.
func TestPollOrdersSnapshotsByExternalUpdatedAt(t *testing.T) {
	env := newPollEnv(t)
	t0 := testNow.Add(-10 * time.Minute)
	env.api.putIssue(1, "stranger", "New title", "New body", "open", t0, nil)
	env.api.putComment(501, 1, "stranger", "Edited", t0)
	env.importIssue(t, 1)
	env.rewindCursors(t, t0)

	// GitHub serves stale copies, updated a minute before the stored ones:
	// within the two-minute overlap, so the next cycle reads them.
	env.api.putIssue(1, "stranger", "Old title", "Old body", "closed", t0.Add(-time.Minute), nil)
	env.api.putComment(501, 1, "stranger", "Original", t0.Add(-time.Minute))
	*env.now = testNow.Add(pollInterval)
	env.poll(t)
	if got := env.issue(t, 1); got.title != "New title" || got.state != "open" ||
		got.description != ExternalIssueDescription("stranger", "New body") {
		t.Fatalf("stale snapshot was applied: %+v", got)
	}
	var content string
	env.f.QueryRow(t, `SELECT c.content FROM comment c JOIN comment_external x ON x.comment_id = c.id
		WHERE x.workspace_id = $1 AND x.external_id = 501`, env.ws).Scan(&content)
	if content != ExternalCommentContent("stranger", "Edited") {
		t.Fatalf("stale comment snapshot was applied: %q", content)
	}

	// A newer edit and close on GitHub is applied; the Multica status is the
	// status mapping's business and does not move here.
	env.mu.Lock()
	env.events = nil
	env.mu.Unlock()
	env.api.putIssue(1, "stranger", "Renamed", "Body v2", "closed", testNow.Add(30*time.Second), nil)
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t)
	got := env.issue(t, 1)
	if got.title != "Renamed" || got.state != "closed" || got.status != "backlog" ||
		got.description != ExternalIssueDescription("stranger", "Body v2") {
		t.Fatalf("newer snapshot not applied: %+v", got)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.events) != 1 || env.events[0].Type != protocol.EventIssueUpdated {
		t.Fatalf("events = %v, want one issue:updated", env.events)
	}
	if payload := env.events[0].Payload.(map[string]any)["issue"].(map[string]any); payload["id"] != got.id || len(payload) != 2 {
		t.Fatalf("issue:updated payload = %v, want only id and workspace_id", payload)
	}
}

// TestPollPagesPastSharedTimestamps reads more updated issues than one page
// holds, with the first page entirely on one second, in a single cycle, and
// syncs the imported ones among them wherever they are.
func TestPollPagesPastSharedTimestamps(t *testing.T) {
	env := newPollEnv(t)
	t0 := testNow.Add(-time.Hour)
	at := func(n int) time.Time {
		if n > 120 {
			return t0.Add(time.Duration(n) * time.Second)
		}
		return t0
	}
	for n := 1; n <= 250; n++ {
		env.api.putIssue(n, "stranger", "Issue "+strconv.Itoa(n), "", "open", at(n), nil)
	}
	env.importIssue(t, 1)
	env.importIssue(t, 250)
	env.rewindCursors(t, t0.Add(-time.Minute))
	// Edited on GitHub without moving updated_at, so only reading them in
	// the stream brings the edit.
	for _, n := range []int{1, 250} {
		env.api.putIssue(n, "stranger", "Edited "+strconv.Itoa(n), "", "open", at(n), nil)
	}
	if stats := env.poll(t); stats.Synced != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := env.issue(t, 1).title + " / " + env.issue(t, 250).title; got != "Edited 1 / Edited 250" {
		t.Fatalf("titles = %s", got)
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 2 {
		t.Fatalf("issues = %d, want the 2 imported", n)
	}
	var cursor time.Time
	env.f.QueryRow(t, `SELECT issues_cursor FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&cursor)
	if !cursor.Equal(t0.Add(250 * time.Second)) {
		t.Fatalf("issues_cursor = %v, want the newest updated_at", cursor)
	}
}

// TestPollGetsPastSharedTimestampsOverCycles is a run of updated issues
// larger than one cycle's page budget, all on one second, with an edit of an
// imported issue and a comment on it after the run. The cursor can only stop
// on that second, so the next cycle must re-read the run and still get past
// it, rather than re-reading the same pages every cycle and never reaching
// the edit or the comments.
func TestPollGetsPastSharedTimestampsOverCycles(t *testing.T) {
	env := newPollEnv(t)
	bulk := testNow.Add(-time.Hour)
	const runLength = 1200 // more than pollPagesPerStream pages of pollPerPage
	for n := 1; n <= runLength; n++ {
		env.api.putIssue(n, "stranger", "Bulk "+strconv.Itoa(n), "", "open", bulk, nil)
	}
	newer := runLength + 1
	env.api.putIssue(newer, "stranger", "Before the run", "", "open", bulk.Add(-time.Hour), nil)
	env.importIssue(t, newer)
	env.rewindCursors(t, bulk.Add(-time.Minute))
	env.api.putIssue(newer, "stranger", "After the run", "", "open", bulk.Add(10*time.Minute), nil)
	env.api.putComment(901, newer, "stranger", "On the imported issue", bulk.Add(11*time.Minute))

	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("first cycle stats = %+v", stats)
	}
	if got := env.issue(t, newer); got.title != "Before the run" {
		t.Fatalf("first cycle got past a run longer than its page budget: %+v", got)
	}
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("second cycle stats = %+v", stats)
	}
	if got := env.issue(t, newer); got.title != "After the run" {
		t.Fatalf("imported issue after two cycles = %+v", got)
	}
	// The issues stream caught up, so the comments stream ran too.
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE workspace_id = $1 AND external_id = 901`, env.ws); n != 1 {
		t.Fatalf("comment on the imported issue not synced after two cycles")
	}
	var cursor time.Time
	env.f.QueryRow(t, `SELECT issues_cursor FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&cursor)
	if !cursor.Equal(bulk.Add(10 * time.Minute)) {
		t.Fatalf("issues_cursor = %v, want the newer issue's updated_at", cursor)
	}

	// Later cycles still catch up every time: the run sits inside no overlap
	// window any more, and none of it enters Multica.
	*env.now = testNow.Add(2 * pollInterval)
	if stats := env.poll(t); stats.Synced != 0 || stats.Failed != 0 {
		t.Fatalf("third cycle stats = %+v, want nothing new", stats)
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 1 {
		t.Fatalf("issues = %d, want only the imported one", n)
	}
}

// TestPollResumesRunsLongerThanAnyCycle shrinks the page size and budget so
// a run on one second is five cycles' worth of pages: no single cycle, nor a
// cycle starting over from the cursor, can read through it. Each cycle must
// continue where the last one stopped, and after a restart has dropped that
// position the stream starts over once and again moves forward, until the
// edit of the imported issue and its comment arrive.
func TestPollResumesRunsLongerThanAnyCycle(t *testing.T) {
	env := newPollEnv(t)
	env.svc.pollLimits.perPage, env.svc.pollLimits.pages = 10, 3
	bulk := testNow.Add(-time.Hour)
	const runLength = 150 // 15 pages of 10, against 3 pages per cycle
	for n := 1; n <= runLength; n++ {
		env.api.putIssue(n, "stranger", "Bulk "+strconv.Itoa(n), "", "open", bulk, nil)
	}
	newer := runLength + 1
	env.api.putIssue(newer, "stranger", "Before the run", "", "open", bulk.Add(-time.Hour), nil)
	env.importIssue(t, newer)
	env.rewindCursors(t, bulk.Add(-time.Minute))
	env.api.putIssue(newer, "stranger", "After the run", "", "open", bulk.Add(time.Minute), nil)
	env.api.putComment(901, newer, "stranger", "On the imported issue", bulk.Add(2*time.Minute))

	issueRequests := func() int {
		n := 0
		for _, r := range env.api.seen() {
			if strings.HasPrefix(r, fmt.Sprintf("/repositories/%d/issues?", pollRepoID)) {
				n++
			}
		}
		return n
	}
	cycles := 0
	for env.issue(t, newer).title != "After the run" {
		cycles++
		// 16 pages at 3 per cycle is 6 cycles; the restart costs the pages
		// read before it once more. A stream that re-reads the same pages
		// every cycle never gets here.
		if cycles > 12 {
			t.Fatalf("edit not synced after %d cycles: the stream is not moving forward", cycles-1)
		}
		if cycles == 3 {
			// A restart: the in-memory position is gone, only the cursor is left.
			env.svc.pollResume.mu.Lock()
			env.svc.pollResume.at = nil
			env.svc.pollResume.mu.Unlock()
		}
		before := issueRequests()
		*env.now = testNow.Add(time.Duration(cycles) * pollInterval)
		if stats := env.poll(t); stats.Failed != 0 {
			t.Fatalf("cycle %d stats = %+v", cycles, stats)
		}
		if sent := issueRequests() - before; sent > 3 {
			t.Fatalf("cycle %d sent %d issue requests, budget is 3", cycles, sent)
		}
	}
	if cycles < 6 {
		t.Fatalf("edit synced after %d cycles; the run should take at least 6", cycles)
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 1 {
		t.Fatalf("issues = %d, want only the imported one", n)
	}
	// The comments stream runs once the issues stream has caught up: in the
	// cycle that synced the edit, or the next.
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE workspace_id = $1 AND external_id = 901`, env.ws); n != 1 {
		*env.now = testNow.Add(time.Duration(cycles+1) * pollInterval)
		env.poll(t)
		if n := env.count(t, `SELECT count(*) FROM comment_external WHERE workspace_id = $1 AND external_id = 901`, env.ws); n != 1 {
			t.Fatal("comment on the imported issue not synced once the issues stream caught up")
		}
	}
}

// TestPollBacksOffWhenRateLimited stops the cycle at the first throttled
// answer and sends nothing until GitHub's Retry-After has passed.
func TestPollBacksOffWhenRateLimited(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	env.api.putIssue(1, "stranger", "Crash on start", "", "open", testNow.Add(-30*time.Second), nil)
	env.api.mu.Lock()
	env.api.throttle = "120"
	env.api.mu.Unlock()

	if stats := env.poll(t); !stats.RateLimited {
		t.Fatalf("stats = %+v, want rate limited", stats)
	}
	sent := len(env.api.seen())
	*env.now = testNow.Add(119 * time.Second)
	env.poll(t)
	if got := len(env.api.seen()); got != sent {
		t.Fatalf("polled during the back-off: %v", env.api.seen()[sent:])
	}

	env.api.mu.Lock()
	env.api.throttle = ""
	env.api.mu.Unlock()
	*env.now = testNow.Add(121 * time.Second)
	if stats := env.poll(t); stats.RateLimited || stats.Synced != 1 {
		t.Fatalf("after the back-off stats = %+v", stats)
	}
}

// TestPollSendsNothingWithUnusableToken keeps an invalid token off GitHub.
func TestPollSendsNothingWithUnusableToken(t *testing.T) {
	env := newPollEnv(t)
	env.svc.state.markRejected(testNow)
	env.poll(t)
	if got := env.api.seen(); len(got) != 0 {
		t.Fatalf("requests with an unusable token: %v", got)
	}
}

// TestPollRecordsRepositoryErrors keeps going after a repository fails and
// leaves the error on its poll state until a clean poll clears it.
func TestPollRecordsRepositoryErrors(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	// GitHub answers the imported issue with a state the poller cannot read.
	env.api.putIssue(1, "stranger", "Crash", "", "unknown", testNow.Add(-30*time.Second), nil)
	if stats := env.poll(t); stats.Failed != 1 || stats.Synced != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	var lastError string
	env.f.QueryRow(t, `SELECT COALESCE(last_error, '') FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&lastError)
	if !strings.Contains(lastError, "issue without number or state") {
		t.Fatalf("last_error = %q", lastError)
	}
	// The failed issue was not skipped: the cursor did not move past it.
	env.api.putIssue(1, "stranger", "Crash on start", "", "open", testNow.Add(-30*time.Second), nil)
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 || stats.Synced != 1 {
		t.Fatalf("retry stats = %+v", stats)
	}
	env.f.QueryRow(t, `SELECT COALESCE(last_error, '') FROM github_repo_sync WHERE workspace_id = $1`, env.ws).Scan(&lastError)
	if lastError != "" {
		t.Fatalf("last_error after a clean poll = %q", lastError)
	}
}

func TestPollJobSpec(t *testing.T) {
	job := (&Service{}).PollJob()
	if job.Name != "github_issues_poll" || job.Cadence != time.Minute || job.RunTimeout >= job.StaleTimeout {
		t.Fatalf("job = %+v", job)
	}
	scopes, err := job.Scopes(context.Background(), testNow)
	if err != nil || len(scopes) != 1 || scopes[0].Kind != "global" {
		t.Fatalf("scopes = %v, %v", scopes, err)
	}
}
