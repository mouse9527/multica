// Added by the mouse9527/multica fork: pull requests of GitHub-native issues
// (server/internal/githubissues). Upstream has no such file.

package handler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GitHubIssueRefs resolves the GitHub-native issues a pull request claims.
type GitHubIssueRefs interface {
	PullRequestIssues(ctx context.Context, workspaceID pgtype.UUID, owner, repo, title, body string) ([]pgtype.UUID, error)
}

// UseGitHubIssuePulls wires the pull request side of GitHub-native issues
// into the handler: links from pull requests to GitHub issues, the static
// token source of the PR snapshot pipeline, and pull request polling into the
// PR mirror. Call it before h.PRRefresh.Start and before the service starts
// polling. With the feature off svc is nil and nothing changes.
func (h *Handler) UseGitHubIssuePulls(svc *githubissues.Service) {
	if svc == nil {
		return
	}
	h.GitHubIssueRefs = svc
	h.PRRefresh.UseStaticSource(svc.SnapshotTransport())
	if h.TaskService != nil {
		h.TaskService.PRSnapshotsEnabled = h.PRRefresh.Enabled()
	}
	svc.SetPullRequestMirror(h)
}

// githubIssueRefs returns the GitHub-native issues the pull request claims,
// and false when they could not be read: the caller then leaves the PR's
// links alone for this event rather than dropping links it cannot confirm.
func (h *Handler) githubIssueRefs(ctx context.Context, wsID pgtype.UUID, in prMirrorInput) ([]pgtype.UUID, bool) {
	if h.GitHubIssueRefs == nil {
		return nil, true
	}
	ids, err := h.GitHubIssueRefs.PullRequestIssues(ctx, wsID, in.RepoOwner, in.RepoName, in.Title, in.Body)
	if err != nil {
		slog.Warn("github: resolve GitHub issue references failed, leaving links unchanged",
			"err", err, "repo", in.RepoOwner+"/"+in.RepoName, "pr_number", in.Number)
		return nil, false
	}
	return ids, true
}

// MirrorPolledPullRequest stores a pull request the GitHub issues poller read
// with the server token, through the same mirror as the App webhook, and asks
// the snapshot pipeline for its CI state with the static token source.
//
// A PR row the App webhook wrote (a real installation id) belongs to the App:
// the poller leaves it alone, and the webhook links GitHub issue references
// itself. A polled PR claims only GitHub issue references, never the
// workspace's "PREFIX-N" identifiers: a repository can be registered in
// several workspaces, and the webhook's guard against an identifier resolving
// in more than one of them (prLinkPolicy) has no counterpart here.
func (h *Handler) MirrorPolledPullRequest(ctx context.Context, wsID pgtype.UUID, pr githubissues.PullRequest) (bool, error) {
	state := "open"
	switch {
	case pr.MergedAt != nil:
		state = "merged"
	case pr.State == "closed":
		state = "closed"
	case pr.Draft:
		state = "draft"
	}
	in := prMirrorInput{
		RepoOwner: pr.Owner, RepoName: pr.Repo, Number: pr.Number,
		Title: pr.Title, Body: pr.Body, State: state,
		HTMLURL: pr.HTMLURL, Branch: pr.HeadRef, HeadSHA: pr.HeadSHA,
		AuthorLogin: pr.AuthorLogin, AuthorAvatarURL: pr.AuthorAvatarURL,
		MergedAt: ghTimeString(pr.MergedAt), ClosedAt: ghTimeString(pr.ClosedAt),
		CreatedAt: ghTimeString(&pr.CreatedAt), UpdatedAt: ghTimeString(&pr.UpdatedAt),
	}
	prevState := ""
	prev, err := h.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: wsID, RepoOwner: pr.Owner, RepoName: pr.Repo, PrNumber: pr.Number,
	})
	switch {
	case err == nil:
		if prev.InstallationID != ghsnapshot.StaticInstallationID || !pr.UpdatedAt.After(prev.PrUpdatedAt.Time) {
			return false, nil
		}
		prevState = prev.State
		// The pulls list carries no line counts or mergeability: keep the
		// stored ones, and drop a verdict that belongs to an older head.
		in.Additions, in.Deletions, in.ChangedFiles = prev.Additions, prev.Deletions, prev.ChangedFiles
		in.ClearMergeableState = prev.HeadSha != pr.HeadSHA
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	if err := h.mirrorPullRequestForWorkspace(ctx, wsID, ghsnapshot.StaticInstallationID, in, prLinkPolicy{unrestricted: true}); err != nil {
		return false, err
	}
	h.PRRefresh.Enqueue(ghsnapshot.StaticInstallationID, pr.Owner, pr.Repo, pr.Number)
	if state == "merged" && prevState != "merged" {
		h.PRRefresh.RefreshConflictWatchers(ctx, ghsnapshot.StaticInstallationID, pr.Owner, pr.Repo, pr.Number)
	}
	return true, nil
}

func ghTimeString(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
