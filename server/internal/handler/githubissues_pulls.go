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
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
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

// githubIssueRefs returns the GitHub-native issues the pull request claims.
// On an error the caller leaves the PR's links alone for this event rather
// than dropping links it cannot confirm.
func (h *Handler) githubIssueRefs(ctx context.Context, wsID pgtype.UUID, in prMirrorInput) ([]pgtype.UUID, error) {
	if h.GitHubIssueRefs == nil {
		return nil, nil
	}
	ids, err := h.GitHubIssueRefs.PullRequestIssues(ctx, wsID, in.RepoOwner, in.RepoName, in.Title, in.Body)
	if err != nil {
		slog.Warn("github: resolve GitHub issue references failed, leaving links unchanged",
			"err", err, "repo", in.RepoOwner+"/"+in.RepoName, "pr_number", in.Number)
		return nil, err
	}
	return ids, nil
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
//
// A snapshot no newer than the stored row is mirrored again rather than
// skipped: its links may have failed last time, and after an error here the
// poller reads the PR again next cycle, which repairs them. It reports a
// change only for a new row or a newer snapshot.
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
	changed, snapshotCurrent := true, false
	prev, err := h.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: wsID, RepoOwner: pr.Owner, RepoName: pr.Repo, PrNumber: pr.Number,
	})
	switch {
	case err == nil:
		if prev.InstallationID != ghsnapshot.StaticInstallationID || pr.UpdatedAt.Before(prev.PrUpdatedAt.Time) {
			return false, nil
		}
		changed = pr.UpdatedAt.After(prev.PrUpdatedAt.Time)
		snapshotCurrent = prev.SnapshotFetchedAt.Valid && prev.SnapshotHeadSha == pr.HeadSHA
		prevState = prev.State
		// The poller reads no line counts or mergeability: keep the stored
		// ones, and drop a verdict that belongs to an older head.
		in.Additions, in.Deletions, in.ChangedFiles = prev.Additions, prev.Deletions, prev.ChangedFiles
		in.ClearMergeableState = prev.HeadSha != pr.HeadSHA
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	if err := h.mirrorPullRequestForWorkspace(ctx, wsID, ghsnapshot.StaticInstallationID, in, prLinkPolicy{unrestricted: true}); err != nil {
		return false, err
	}
	if changed || !snapshotCurrent {
		h.PRRefresh.Enqueue(ghsnapshot.StaticInstallationID, pr.Owner, pr.Repo, pr.Number)
	}
	if state == "merged" && prevState != "merged" {
		h.PRRefresh.RefreshConflictWatchers(ctx, ghsnapshot.StaticInstallationID, pr.Owner, pr.Repo, pr.Number)
	}
	return changed, nil
}

func ghTimeString(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// prAutoCompleteGitHub is the PR auto-complete decision for a GitHub-native
// issue. GitHub is the only writer of its status: a merge that closes it on
// GitHub reaches Multica through the poller's status mapping, so Multica never
// moves it on its own. Clients render nothing for a state they do not know.
const prAutoCompleteGitHub = "github"

// statusFromGitHub reports whether GitHub owns the issue's status. A failed
// read is an error, not "local": the answer gates a status write.
func (h *Handler) statusFromGitHub(ctx context.Context, issue db.Issue) (bool, error) {
	if !issuekey.Enabled() {
		return false, nil
	}
	_, err := h.Queries.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}
