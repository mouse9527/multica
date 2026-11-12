package githubissues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// serveIssue answers GET /repositories/{id}/issues/{number} and its
// /comments list the way GitHub documents them: comments ascending by
// creation, paged by per_page and page. It reports whether it answered.
func (f *fakeIssuesAPI) serveIssue(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, fmt.Sprintf("/repositories/%d/issues/", pollRepoID))
	if !ok {
		return false
	}
	numberPart, comments := strings.CutSuffix(rest, "/comments")
	number, err := strconv.Atoi(numberPart)
	if err != nil {
		return false
	}
	if reply, ok := f.issueReplies[number]; ok && !comments {
		if reply.location != "" {
			w.Header().Set("Location", reply.location)
		}
		w.WriteHeader(reply.status)
		return true
	}
	issue, found := f.issues[number]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return true
	}
	if !comments {
		_ = json.NewEncoder(w).Encode(issue)
		return true
	}
	if fault := f.commentsFault; fault.page != 0 && r.URL.Query().Get("page") == strconv.Itoa(fault.page) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(fault.status)
		_, _ = w.Write([]byte(`{"message":"failure"}`))
		return true
	}
	suffix := fmt.Sprintf("/issues/%d", number)
	var items []map[string]any
	for _, c := range f.comments {
		if strings.HasSuffix(c["issue_url"].(string), suffix) {
			items = append(items, c)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["id"].(int64) < items[j]["id"].(int64) })
	q := r.URL.Query()
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page, _ := strconv.Atoi(q.Get("page"))
	if perPage <= 0 || page <= 0 {
		f.t.Errorf("comments listed without paging: %s", r.URL.RawQuery)
		perPage, page = 30, 1
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	_ = json.NewEncoder(w).Encode(items[start:end])
	return true
}

// countPath counts the requests to path, whatever their query.
func (f *fakeIssuesAPI) countPath(path string) int {
	n := 0
	for _, req := range f.seen() {
		if p, _, _ := strings.Cut(req, "?"); p == path {
			n++
		}
	}
	return n
}

func (env *pollEnv) loadIssue(t *testing.T, id string) db.Issue {
	t.Helper()
	issue, err := db.New(env.svc.db).GetIssue(context.Background(), util.MustParseUUID(id))
	if err != nil {
		t.Fatalf("load issue %s: %v", id, err)
	}
	return issue
}

// view opens the issue as the handler hook does, then waits for the
// background refresh it may have started.
func (env *pollEnv) view(issue db.Issue) {
	env.svc.RefreshIssueOnView(context.Background(), issue)
	env.svc.refreshing.Wait()
}

// TestRefreshOnViewAtMostOncePerInterval is the acceptance test: opening the
// same GitHub issue again and again within 15 seconds calls GitHub once, and
// the refresh stores what changed on GitHub since the last poll.
func TestRefreshOnViewAtMostOncePerInterval(t *testing.T) {
	env := newPollEnv(t)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash on start", "It crashes.", "open", t0, nil)
	env.api.putComment(501, 1, "stranger", "Same here", t0.Add(time.Minute))
	issue := env.loadIssue(t, env.importIssue(t, 1).id)
	issuePath := fmt.Sprintf("/repositories/%d/issues/1", pollRepoID)
	imported, importedComments := env.api.countPath(issuePath), env.api.countPath(issuePath+"/comments")

	// Someone edits the issue and comments on GitHub; no poll has run since.
	env.api.putIssue(1, "stranger", "Crash on start (macOS)", "It crashes.", "open", testNow.Add(-10*time.Second), nil)
	env.api.putComment(502, 1, "stranger", "Me too", testNow.Add(-5*time.Second))

	for range 5 {
		env.view(issue)
	}
	*env.now = testNow.Add(refreshInterval - time.Second)
	env.view(issue)
	if got := env.api.countPath(issuePath) - imported; got != 1 {
		t.Fatalf("GET issue requests within the interval = %d, want 1", got)
	}
	if got := env.api.countPath(issuePath+"/comments") - importedComments; got != 1 {
		t.Fatalf("GET comments requests within the interval = %d, want 1", got)
	}
	if got := env.issue(t, 1); got.title != "Crash on start (macOS)" {
		t.Fatalf("title after refresh = %q", got.title)
	}
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE workspace_id = $1 AND external_id = 502`, env.ws); n != 1 {
		t.Fatal("new GitHub comment not stored by the refresh")
	}
	if n := env.count(t, `SELECT count(*) FROM comment WHERE workspace_id = $1`, env.ws); n != 2 {
		t.Fatalf("comments = %d, want 2 (no duplicate of the imported one)", n)
	}

	// Once the interval has passed, the next view refreshes again.
	*env.now = testNow.Add(refreshInterval)
	env.view(issue)
	if got := env.api.countPath(issuePath) - imported; got != 2 {
		t.Fatalf("GET issue requests after the interval = %d, want 2", got)
	}
}

// TestRefreshOnViewConcurrentViewsShareOneRequest opens one issue from many
// goroutines at once: one refresh runs, the rest return without sending.
func TestRefreshOnViewConcurrentViewsShareOneRequest(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	issue := env.loadIssue(t, env.importIssue(t, 1).id)
	issuePath := fmt.Sprintf("/repositories/%d/issues/1", pollRepoID)
	imported := env.api.countPath(issuePath)

	done := make(chan struct{})
	for range 20 {
		go func() {
			env.svc.RefreshIssueOnView(context.Background(), issue)
			done <- struct{}{}
		}()
	}
	for range 20 {
		<-done
	}
	env.svc.refreshing.Wait()
	if got := env.api.countPath(issuePath) - imported; got != 1 {
		t.Fatalf("GET issue requests = %d, want 1", got)
	}
}

// TestRefreshOnViewSendsNothing covers the views that must not reach GitHub:
// a local issue, an unusable token, and a poll pause after throttling.
func TestRefreshOnViewSendsNothing(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.importIssue(t, 1)
	before := len(env.api.seen())

	local := env.loadIssue(t, env.f.Issue(t, "local issue"))
	env.view(local)

	github := env.loadIssue(t, env.issue(t, 1).id)
	env.svc.pollGate.pause(testNow.Add(time.Minute))
	env.view(github)
	env.svc.pollGate = pollGate{}

	env.svc.state.markRejected(testNow)
	env.view(github)

	if got := env.api.seen()[before:]; len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
	var off *Service
	off.RefreshIssueOnView(context.Background(), github)
}

// TestRefreshOnViewThrottledPausesPolling: a 429 on a view pauses the poller
// and later views for the wait GitHub asked for.
func TestRefreshOnViewThrottledPausesPolling(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	issue := env.loadIssue(t, env.importIssue(t, 1).id)

	env.api.mu.Lock()
	env.api.throttle = "120"
	env.api.mu.Unlock()
	env.view(issue)
	if !env.svc.pollGate.paused(testNow.Add(time.Minute)) {
		t.Fatal("polling not paused after a throttled refresh")
	}
}

func TestRefreshGate(t *testing.T) {
	var g refreshGate
	a := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	b := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	if !g.admit(a, testNow) {
		t.Fatal("first view not admitted")
	}
	if g.admit(a, testNow) {
		t.Fatal("second view admitted while the first refresh runs")
	}
	if !g.admit(b, testNow) {
		t.Fatal("another issue held back")
	}
	// Still running past the interval: no second refresh of the same issue.
	if g.admit(a, testNow.Add(refreshInterval)) {
		t.Fatal("admitted while in flight")
	}
	g.done(a)
	g.done(b)
	if g.admit(a, testNow.Add(refreshInterval-time.Nanosecond)) {
		t.Fatal("admitted within the interval")
	}
	if !g.admit(a, testNow.Add(refreshInterval)) {
		t.Fatal("not admitted once the interval passed")
	}
	if _, kept := g.started[b]; kept {
		t.Fatal("expired entry kept")
	}
}
