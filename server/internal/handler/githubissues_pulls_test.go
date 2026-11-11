// Added by the mouse9527/multica fork: pull requests of GitHub-native issues.

package handler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeIssueRefs answers PullRequestIssues with fixed issues, or an error.
type fakeIssueRefs struct {
	ids  []pgtype.UUID
	err  error
	seen []string
}

func (f *fakeIssueRefs) PullRequestIssues(_ context.Context, _ pgtype.UUID, owner, repo, title, body string) ([]pgtype.UUID, error) {
	f.seen = append(f.seen, owner+"/"+repo+" "+title+" | "+body)
	return f.ids, f.err
}

// pullRequestTestRepo returns a repository name of its own and removes the
// PR rows of it, and their links, when the test ends.
func pullRequestTestRepo(t *testing.T) string {
	t.Helper()
	repo := "pulls-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE pull_request_id IN
			(SELECT id FROM github_pull_request WHERE workspace_id = $1 AND repo_name = $2)`, testWorkspaceID, repo)
		_, _ = testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1 AND repo_name = $2`, testWorkspaceID, repo)
	})
	return repo
}

func polledPullRequest(repo string, number int32, body string, updated time.Time) githubissues.PullRequest {
	return githubissues.PullRequest{
		Owner: "acme", Repo: repo, Number: number, State: "open",
		HTMLURL: fmt.Sprintf("https://github.com/acme/%s/pull/%d", repo, number),
		Title:   "Handle empty config", Body: body,
		CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
		HeadRef: "empty-config", HeadSHA: fmt.Sprintf("%040d", updated.Unix()), AuthorLogin: "contributor",
	}
}

func storedPullRequest(t *testing.T, repo string, number int32) db.GithubPullRequest {
	t.Helper()
	pr, err := testHandler.Queries.GetGitHubPullRequest(context.Background(), db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), RepoOwner: "acme", RepoName: repo, PrNumber: number,
	})
	if err != nil {
		t.Fatalf("read PR row: %v", err)
	}
	return pr
}

// The poller's pull requests link the GitHub issues they close, keep a link
// when the references cannot be read, and drop it once an open PR no longer
// claims the issue.
func TestMirrorPolledPullRequestLinksGitHubIssues(t *testing.T) {
	refs := &fakeIssueRefs{}
	h := *testHandler
	h.GitHubIssueRefs = refs
	repo := pullRequestTestRepo(t)
	issueID := dbfx.Issue(t, "GitHub issue closed by a polled PR")
	refs.ids = []pgtype.UUID{parseUUID(issueID)}
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	changed, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), polledPullRequest(repo, 7, "Closes #1", t0))
	if err != nil || !changed {
		t.Fatalf("mirror: %v %v", changed, err)
	}
	pr := storedPullRequest(t, repo, 7)
	if pr.InstallationID != ghsnapshot.StaticInstallationID || pr.State != "open" || pr.Title != "Handle empty config" {
		t.Fatalf("PR row = %+v", pr)
	}
	if len(refs.seen) != 1 || refs.seen[0] != "acme/"+repo+" Handle empty config | Closes #1" {
		t.Fatalf("references read from %q", refs.seen)
	}
	if n := linkedPRCountForTest(t, issueID); n != 1 {
		t.Fatalf("issue has %d PRs, want 1", n)
	}
	// The polled PR claims no "PREFIX-N" identifier, only GitHub references.
	if prs := listIssuePRsForTest(t, issueID).PullRequests; len(prs) != 1 || prs[0].LinkSource != "auto" {
		t.Fatalf("issue PRs = %+v", prs)
	}

	// The same snapshot again changes nothing.
	if changed, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), polledPullRequest(repo, 7, "Closes #1", t0)); err != nil || changed {
		t.Fatalf("re-mirror of the same snapshot: %v %v", changed, err)
	}

	// References that cannot be read leave the link alone, and the error
	// keeps the poller on this PR.
	refs.ids, refs.err = nil, errors.New("database unavailable")
	if _, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), polledPullRequest(repo, 7, "", t0.Add(time.Minute))); err == nil {
		t.Fatal("a failed reference read was reported as stored")
	}
	if n := linkedPRCountForTest(t, issueID); n != 1 {
		t.Fatalf("link dropped on a failed reference read: %d PRs", n)
	}

	// The open PR no longer closes the issue: the automatic link goes.
	refs.err = nil
	if _, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), polledPullRequest(repo, 7, "", t0.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if n := linkedPRCountForTest(t, issueID); n != 0 {
		t.Fatalf("issue still has %d PRs after the reference was removed", n)
	}
}

// A PR row the GitHub App webhook wrote stays the App's: the poller does not
// overwrite it. A polled row keeps the line counts the list does not carry.
func TestMirrorPolledPullRequestLeavesAppRows(t *testing.T) {
	h := *testHandler
	repo := pullRequestTestRepo(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	app := (&ghPullRequestPayload{}).mirrorInput()
	app.RepoOwner, app.RepoName, app.Number, app.Title, app.State = "acme", repo, 8, "From the App", "open"
	app.HTMLURL, app.HeadSHA = "https://github.com/acme/"+repo+"/pull/8", "abc"
	app.CreatedAt, app.UpdatedAt = t0.Format(time.RFC3339), t0.Format(time.RFC3339)
	app.Additions = 12
	if err := h.mirrorPullRequestForWorkspace(ctx, parseUUID(testWorkspaceID), 4242, app, prLinkPolicy{unrestricted: true}); err != nil {
		t.Fatal(err)
	}
	if changed, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), polledPullRequest(repo, 8, "", t0.Add(time.Minute))); err != nil || changed {
		t.Fatalf("poller wrote an App row: %v %v", changed, err)
	}
	if pr := storedPullRequest(t, repo, 8); pr.InstallationID != 4242 || pr.Title != "From the App" {
		t.Fatalf("App row changed: %+v", pr)
	}

	// A polled row: counts survive a later polled snapshot.
	if _, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), polledPullRequest(repo, 9, "", t0)); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE github_pull_request SET additions = 5, deletions = 2, changed_files = 1
		WHERE workspace_id = $1 AND repo_name = $2 AND pr_number = 9`, testWorkspaceID, repo); err != nil {
		t.Fatal(err)
	}
	merged := polledPullRequest(repo, 9, "", t0.Add(time.Minute))
	mergedAt := t0.Add(time.Minute)
	merged.State, merged.MergedAt, merged.ClosedAt = "closed", &mergedAt, &mergedAt
	if _, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), merged); err != nil {
		t.Fatal(err)
	}
	pr := storedPullRequest(t, repo, 9)
	if pr.State != "merged" || pr.Additions != 5 || pr.Deletions != 2 || pr.ChangedFiles != 1 {
		t.Fatalf("polled row = state %s +%d -%d files %d", pr.State, pr.Additions, pr.Deletions, pr.ChangedFiles)
	}
}

// A link that failed is made by the next cycle, which reads the same, unedited
// PR again: the PR row being stored does not end the work.
func TestMirrorPolledPullRequestRepairsFailedLink(t *testing.T) {
	refs := &fakeIssueRefs{err: errors.New("connection reset")}
	h := *testHandler
	h.GitHubIssueRefs = refs
	repo := pullRequestTestRepo(t)
	issueID := dbfx.Issue(t, "GitHub issue whose link failed once")
	ctx := context.Background()
	pr := polledPullRequest(repo, 10, "Closes #1", time.Now().Add(-time.Hour).Truncate(time.Second))

	if _, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), pr); err == nil {
		t.Fatal("first mirror reported success without its links")
	}
	storedPullRequest(t, repo, 10)
	if n := linkedPRCountForTest(t, issueID); n != 0 {
		t.Fatalf("issue has %d PRs before the links were read", n)
	}

	refs.ids, refs.err = []pgtype.UUID{parseUUID(issueID)}, nil
	changed, err := h.MirrorPolledPullRequest(ctx, parseUUID(testWorkspaceID), pr)
	if err != nil || changed {
		t.Fatalf("second mirror of the same snapshot: changed=%v err=%v, want the link repaired without a change", changed, err)
	}
	if n := linkedPRCountForTest(t, issueID); n != 1 {
		t.Fatalf("issue has %d PRs after the next cycle, want 1", n)
	}
}

// A merged PR does not move a GitHub-native issue: GitHub writes its status,
// and the poller brings it in. A local issue still moves on merge.
func TestPRMergeLeavesGitHubIssueStatusToGitHub(t *testing.T) {
	setIssueKeysEnabledForTest(t, true)
	setWorkspacePRMergeStatusForTest(t, "done")
	repo := pullRequestTestRepo(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	githubIssue := dbfx.Issue(t, "GitHub issue closed by a merged PR", testutil.Cols{"status": "in_progress"})
	linkGitHubIssue(t, githubIssue, "acme/"+repo, 1)
	local := dbfx.Issue(t, "Local issue closed by a merged PR", testutil.Cols{"status": "in_progress"})
	localIdent := fmt.Sprintf("HAN-%d", issueKeyTestNumber(t, local))
	t.Cleanup(func() {
		for _, id := range []string{githubIssue, local} {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id = $1`, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_pr_automation WHERE issue_id = $1`, id)
		}
	})

	h := *testHandler
	h.GitHubIssueRefs = &fakeIssueRefs{ids: []pgtype.UUID{parseUUID(githubIssue)}}
	merged := (&ghPullRequestPayload{}).mirrorInput()
	merged.RepoOwner, merged.RepoName, merged.Number = "acme", repo, 11
	merged.Title, merged.Body, merged.State = "Fix crash", "Closes #1\nCloses "+localIdent, "merged"
	merged.HTMLURL, merged.HeadSHA = "https://github.com/acme/"+repo+"/pull/11", "abc"
	merged.CreatedAt, merged.UpdatedAt, merged.MergedAt = t0.Format(time.RFC3339), t0.Format(time.RFC3339), t0.Format(time.RFC3339)
	merged.Idents = prClaimedIdentifiers(merged.Title, merged.Body, "")
	if err := h.mirrorPullRequestForWorkspace(ctx, parseUUID(testWorkspaceID), 4343, merged, prLinkPolicy{unrestricted: true}); err != nil {
		t.Fatal(err)
	}

	if n := linkedPRCountForTest(t, githubIssue); n != 1 {
		t.Fatalf("GitHub issue has %d PRs, want 1", n)
	}
	if got := issueStatusForTest(t, githubIssue); got != "in_progress" {
		t.Fatalf("GitHub issue moved to %q by a merge; GitHub writes its status", got)
	}
	if got := prAutoCompleteStateForTest(t, githubIssue).State; got != prAutoCompleteGitHub {
		t.Fatalf("GitHub issue auto-complete state = %q, want %q", got, prAutoCompleteGitHub)
	}
	if got := issueStatusForTest(t, local); got != "done" {
		t.Fatalf("local issue status after merge = %q, want done", got)
	}
}
