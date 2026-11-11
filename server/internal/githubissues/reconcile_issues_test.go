package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

const otherRepoID = 43

// serveOtherRepo makes acme/gadgets (repository 43) a second repository of
// the fake, holding issues: no list has anything new, and each issue reads
// as given.
func (env *pollEnv) serveOtherRepo(t *testing.T, issues map[int]map[string]any) {
	t.Helper()
	env.f.Insert(t, "project_resource", testutil.Cols{
		"project_id": env.project, "workspace_id": env.ws,
		"resource_type": "github_repo", "resource_ref": `{"url":"https://github.com/acme/gadgets"}`,
	})
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	env.api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		base := fmt.Sprintf("/repositories/%d/issues", otherRepoID)
		switch r.URL.Path {
		case "/repos/acme/gadgets":
			_, _ = fmt.Fprintf(w, `{"id":%d}`, otherRepoID)
		case base, base + "/comments":
			_, _ = w.Write([]byte(`[]`))
		default:
			var n int
			if _, err := fmt.Sscanf(r.URL.Path, base+"/%d", &n); err != nil || fmt.Sprintf("%s/%d", base, n) != r.URL.Path {
				return false
			}
			issue, ok := issues[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			_ = json.NewEncoder(w).Encode(issue)
		}
		return true
	}
}

func (env *pollEnv) replyToIssue(number, status int, location string) {
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if env.api.issueReplies == nil {
		env.api.issueReplies = map[int]issueReply{}
	}
	env.api.issueReplies[number] = issueReply{status: status, location: location}
}

type externalRow struct {
	repoID      int64
	repo        string
	number      int
	stateReason string
}

func (env *pollEnv) external(t *testing.T, issueID string) externalRow {
	t.Helper()
	var got externalRow
	env.f.QueryRow(t, `SELECT external_repo_id, external_repo, external_number, COALESCE(state_reason, '')
		FROM issue_external WHERE issue_id = $1`, issueID).Scan(&got.repoID, &got.repo, &got.number, &got.stateReason)
	return got
}

// TestReconcileFindsDeletedAndTransferredIssues is the daily reconcile: each
// tracked issue is read on its own once a day. It brings in a change the
// "since" lists missed, marks deleted issues, re-attaches an issue
// transferred to another registered repository, and marks one transferred
// elsewhere or onto an issue already tracked. Nothing local is deleted, and
// an issue found gone is never read again.
func TestReconcileFindsDeletedAndTransferredIssues(t *testing.T) {
	env := newPollEnv(t)
	ctx := context.Background()
	t0 := testNow.Add(-time.Hour)
	for n := 1; n <= 6; n++ {
		env.api.putIssue(n, "stranger", fmt.Sprintf("Issue %d", n), "body", "open", t0, nil)
	}
	env.serveOtherRepo(t, map[int]map[string]any{7: {
		"id": 2007, "node_id": "I_7", "number": 7, "title": "Issue 4", "body": "body", "state": "open",
		"state_reason": nil, "updated_at": testNow.Format(time.RFC3339), "user": map[string]any{"login": "stranger"},
	}})
	ids := map[int]string{}
	for n := 1; n <= 6; n++ {
		ids[n] = env.importIssue(t, n).id
	}
	ws := util.MustParseUUID(env.ws)
	if _, err := env.svc.RepoClient(ctx, ws, "acme", "gadgets"); err != nil {
		t.Fatalf("pin acme/gadgets: %v", err)
	}
	// Synced at import: nothing is due for a day.
	reads := func() int {
		n := env.api.countPath(fmt.Sprintf("/repositories/%d/issues/7", otherRepoID))
		for number := range ids {
			n += env.api.countPath(fmt.Sprintf("/repositories/%d/issues/%d", pollRepoID, number))
		}
		return n
	}
	before := reads()
	env.poll(t)
	if got := reads(); got != before {
		t.Fatalf("issue reads within a day of the last sync = %d, want 0", got-before)
	}

	// An edit the lists did not return (older than the cursor window).
	env.api.putIssue(1, "stranger", "Issue 1 (edited)", "body", "open", t0.Add(time.Minute), nil)
	env.replyToIssue(2, http.StatusGone, "")
	env.api.mu.Lock()
	delete(env.api.issues, 3) // 404
	env.api.mu.Unlock()
	env.replyToIssue(4, http.StatusMovedPermanently, fmt.Sprintf("https://api.github.com/repositories/%d/issues/7", otherRepoID))
	env.replyToIssue(5, http.StatusMovedPermanently, "https://api.github.com/repositories/99/issues/3")
	// Onto the issue 4 moves to, which is then tracked.
	env.replyToIssue(6, http.StatusMovedPermanently, fmt.Sprintf("https://api.github.com/repositories/%d/issues/7", otherRepoID))

	env.lastSyncedDayAgo(t)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("reconcile cycle stats = %+v", stats)
	}
	if got := env.issue(t, 1); got.title != "Issue 1 (edited)" {
		t.Fatalf("issue 1 = %+v, want the edit brought in", got)
	}
	want := map[int]externalRow{
		1: {pollRepoID, "acme/widgets", 1, ""},
		2: {pollRepoID, "acme/widgets", 2, goneDeleted},
		3: {pollRepoID, "acme/widgets", 3, goneDeleted},
		4: {otherRepoID, "acme/gadgets", 7, ""},
		5: {pollRepoID, "acme/widgets", 5, goneTransferred},
		6: {pollRepoID, "acme/widgets", 6, goneTransferred},
	}
	for n, w := range want {
		if got := env.external(t, ids[n]); got != w {
			t.Errorf("issue %d link = %+v, want %+v", n, got, w)
		}
	}
	if n := env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`, env.ws); n != 6 {
		t.Fatalf("local issues = %d, want all 6 kept", n)
	}
	if n := env.count(t, `SELECT count(*) FROM github_repo_sync WHERE workspace_id = $1 AND repo_id = $2
		AND last_full_reconcile_at IS NOT NULL`, env.ws, pollRepoID); n != 1 {
		t.Fatal("completed pass not recorded")
	}

	// Checked today, and gone issues never again.
	before = reads()
	env.poll(t)
	env.poll(t)
	if got := reads(); got != before {
		t.Fatalf("issue reads after the pass = %d, want 0", got-before)
	}

	// A gone issue goes on as a local one: writes stay local, any status
	// goes, and opening it reads nothing.
	member := Actor{Type: "member", ID: util.MustParseUUID(env.owner)}
	gone := env.loadIssue(t, ids[2])
	title := "Renamed locally"
	if w, err := env.svc.BeginIssueUpdate(ctx, member, gone, IssueChange{Title: &title}); w != nil || err != nil {
		t.Fatalf("update of a deleted issue: %v %v, want a local write", w, err)
	}
	if w, err := env.svc.BeginCommentCreate(ctx, member, gone, "still here"); w != nil || err != nil {
		t.Fatalf("comment on a deleted issue: %v %v, want a local write", w, err)
	}
	project := util.MustParseUUID(env.project)
	if err := env.svc.CheckGitHubStatus(ctx, gone, project, "in_progress"); err != nil {
		t.Fatalf("status of a deleted issue: %v", err)
	}
	var unmapped *UnmappedStatusError
	if err := env.svc.CheckGitHubStatus(ctx, env.loadIssue(t, ids[1]), project, "in_progress"); !errors.As(err, &unmapped) {
		t.Fatalf("status of a live issue: %v, want UnmappedStatusError", err)
	}
	env.view(gone)
	if got := reads(); got != before {
		t.Fatal("opening a deleted issue read it from GitHub")
	}
	if n := env.count(t, `SELECT count(*) FROM github_write_intent WHERE workspace_id = $1`, env.ws); n != 0 {
		t.Fatalf("writes to gone issues left %d intents", n)
	}
}

// A repository GitHub cannot answer for (5xx) stops the reconcile there:
// nothing is marked, the error is recorded, and the next cycle tries again.
func TestReconcileStopsOnUnexpectedAnswers(t *testing.T) {
	env := newPollEnv(t)
	env.api.putIssue(1, "stranger", "Issue 1", "body", "open", testNow.Add(-time.Hour), nil)
	id := env.importIssue(t, 1).id
	env.replyToIssue(1, http.StatusBadGateway, "")
	env.lastSyncedDayAgo(t)
	if stats := env.poll(t); stats.Failed != 1 {
		t.Fatalf("stats = %+v, want the repository failed", stats)
	}
	if got := env.external(t, id); got.stateReason != "" {
		t.Fatalf("link after a 5xx = %+v, want unmarked", got)
	}
	if n := env.count(t, `SELECT count(*) FROM github_repo_sync WHERE workspace_id = $1 AND last_error LIKE '%unexpected status 502%'`, env.ws); n != 1 {
		t.Fatal("reconcile error not recorded")
	}
	env.api.mu.Lock()
	delete(env.api.issueReplies, 1)
	env.api.mu.Unlock()
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("retry stats = %+v", stats)
	}
	if n := env.count(t, `SELECT count(*) FROM issue_external WHERE issue_id = $1
		AND external_synced_at > now() - interval '1 minute'`, id); n != 1 {
		t.Fatal("the check was not recorded")
	}
}

// lastSyncedDayAgo makes every tracked issue due for the reconcile: last
// synced (by the database clock, as GitHub syncs stamp it) over a day ago.
func (env *pollEnv) lastSyncedDayAgo(t *testing.T) {
	t.Helper()
	env.f.Exec(t, `UPDATE issue_external SET external_synced_at = now() - interval '25 hours' WHERE workspace_id = $1`, env.ws)
}

func TestParseIssueLocation(t *testing.T) {
	cases := map[string]struct {
		repo   int64
		number int32
		ok     bool
	}{
		"https://api.github.com/repositories/43/issues/7":           {43, 7, true},
		"/repositories/43/issues/7":                                 {43, 7, true},
		"https://api.github.com/repos/acme/gadgets/issues/7":        {},
		"https://api.github.com/repositories/43/issues/7/comments":  {},
		"https://api.github.com/repositories/0/issues/7":            {},
		"https://api.github.com/repositories/43/issues/0":           {},
		"https://api.github.com/repositories/43/issues/99999999999": {},
		"": {},
	}
	for location, want := range cases {
		repo, number, ok := parseIssueLocation(location)
		if repo != want.repo || number != want.number || ok != want.ok {
			t.Errorf("parseIssueLocation(%q) = %d %d %v, want %d %d %v", location, repo, number, ok, want.repo, want.number, want.ok)
		}
	}
}
