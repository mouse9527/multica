// Modified for the mouse9527/multica fork: GitHub-native issues.

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeGitHubToken is shaped like a classic PAT but is not a real credential.
const fakeGitHubToken = "ghp_FAKEtokenFORtestsONLY0123456789abcd"

func syncExternalCommentForTest(t *testing.T, svc *githubissues.Service, issueID string, externalID int64) string {
	t.Helper()
	res, err := svc.SyncExternalComment(context.Background(), githubissues.ExternalComment{
		WorkspaceID: parseUUID(testWorkspaceID),
		IssueID:     parseUUID(issueID),
		ExternalID:  externalID,
		Login:       "octocat",
		Body:        "written on GitHub",
		UpdatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("sync external comment: %v", err)
	}
	id := util.UUIDToString(res.Comment.ID)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM comment_external WHERE comment_id = $1`, id)
		_, _ = testPool.Exec(ctx, `DELETE FROM comment WHERE id = $1`, id)
	})
	return id
}

// Comments written on GitHub by people outside the workspace are read-only in
// Multica, even for the workspace owner. Other comments are unaffected.
func TestExternalCommentsAreReadOnly(t *testing.T) {
	svc := githubissues.New(githubissues.LoadConfig(func(key string) string {
		return map[string]string{
			githubissues.EnvEnabled:        "true",
			githubissues.EnvToken:          fakeGitHubToken,
			githubissues.EnvTokenExpiresAt: "never",
		}[key]
	}), githubissues.Options{DB: testPool})
	h := *testHandler
	h.ExternalComments = svc

	issueID := dbfx.Issue(t, "external comment read-only")
	external := syncExternalCommentForTest(t, svc, issueID, time.Now().UnixNano())
	var before string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, external).Scan(&before)

	edit := func(handler *Handler, id string) *testutil.Response {
		return testutil.Call(t, handler.UpdateComment, withURLParam(newRequest(http.MethodPut, "/api/comments/"+id, map[string]any{
			"content": "rewritten in Multica",
		}), "commentId", id))
	}
	remove := func(handler *Handler, id string) *testutil.Response {
		return testutil.Call(t, handler.DeleteComment, withURLParam(newRequest(http.MethodDelete, "/api/comments/"+id, nil), "commentId", id))
	}

	edit(&h, external).Want(http.StatusForbidden)
	remove(&h, external).Want(http.StatusForbidden)
	var after string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1 AND deleted_at IS NULL`, external).Scan(&after)
	if after != before {
		t.Fatalf("external comment changed: %q -> %q", before, after)
	}

	// A member's comment on the same issue stays editable and deletable.
	member := dbfx.Comment(t, issueID, "member comment")
	edit(&h, member).Want(http.StatusOK)
	remove(&h, member).Want(http.StatusNoContent)

	// A platform notice (system author, no GitHub mirror) keeps upstream
	// behavior: the owner may edit it.
	notice := dbfx.Comment(t, issueID, "platform notice", testutil.Cols{
		"author_type": "system", "author_id": "00000000-0000-0000-0000-000000000000",
	})
	edit(&h, notice).Want(http.StatusOK)
}

// With the feature off the hook is absent and the handler behaves as
// upstream, whatever rows a previous run left behind.
func TestExternalCommentHookOffMatchesUpstream(t *testing.T) {
	svc := githubissues.New(githubissues.LoadConfig(func(key string) string {
		return map[string]string{
			githubissues.EnvEnabled:        "true",
			githubissues.EnvToken:          fakeGitHubToken,
			githubissues.EnvTokenExpiresAt: "never",
		}[key]
	}), githubissues.Options{DB: testPool})
	if testHandler.ExternalComments != nil {
		t.Fatal("the shared test handler must not carry the fork hook")
	}
	issueID := dbfx.Issue(t, "external comment feature off")
	external := syncExternalCommentForTest(t, svc, issueID, time.Now().UnixNano())

	testutil.Call(t, testHandler.UpdateComment, withURLParam(newRequest(http.MethodPut, "/api/comments/"+external, map[string]any{
		"content": "edited while the feature is off",
	}), "commentId", external)).Want(http.StatusOK)
}

type recordedViews struct{ issues []string }

func (v *recordedViews) RefreshIssueOnView(_ context.Context, issue db.Issue) {
	v.issues = append(v.issues, util.UUIDToString(issue.ID))
}

// Opening an issue, its comments or its timeline asks for a refresh of that
// issue; a request that cannot load the issue asks for nothing.
func TestIssueViewsAskForRefresh(t *testing.T) {
	views := &recordedViews{}
	h := *testHandler
	h.IssueViews = views
	issueID := dbfx.Issue(t, "refresh on view")

	for _, call := range []struct {
		handler func(http.ResponseWriter, *http.Request)
		path    string
	}{
		{h.GetIssue, "/api/issues/" + issueID},
		{h.ListComments, "/api/issues/" + issueID + "/comments"},
		{h.ListTimeline, "/api/issues/" + issueID + "/timeline"},
	} {
		testutil.Call(t, call.handler, withURLParam(newRequest(http.MethodGet, call.path, nil), "id", issueID)).Want(http.StatusOK)
	}
	if len(views.issues) != 3 || views.issues[0] != issueID || views.issues[1] != issueID || views.issues[2] != issueID {
		t.Fatalf("refreshed issues = %v, want %s three times", views.issues, issueID)
	}

	missing := "00000000-0000-0000-0000-00000000dead"
	testutil.Call(t, h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+missing, nil), "id", missing)).Want(http.StatusNotFound)
	if len(views.issues) != 3 {
		t.Fatalf("refresh asked for an issue that failed to load: %v", views.issues)
	}
	if testHandler.IssueViews != nil {
		t.Fatal("the shared test handler must not carry the fork hook")
	}
}
