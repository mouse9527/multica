package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IssueChange is the part of an issue update that GitHub holds. A nil field
// is not being changed.
type IssueChange struct {
	Title       *string
	Description *string
	Status      *string
	// ProjectID is the project the issue is in after the update; its
	// status_label_map decides the status label.
	ProjectID pgtype.UUID
}

// NewIssue is an issue to open on GitHub from Multica.
type NewIssue struct {
	// Repo is "owner/name" of a repository registered in the workspace.
	Repo        string
	Title       string
	Description string
	Status      string
	ProjectID   pgtype.UUID
}

// statusTarget is how a Multica status is written on GitHub: the state, the
// state_reason when closed, and the status label while open.
type statusTarget struct {
	state  string
	reason string
	label  string
	// mapping is the project's status mapping; a status change removes
	// every label it maps before adding label.
	mapping statusMapping
}

// statusTargetFor maps status the way status.go reads it back: a done-category
// status closes the issue as completed, a cancelled-category one as
// not_planned, the entry status is an open issue without a status label, and
// any other status is open with the label the project maps it to. A status
// the mapping cannot show is refused (UnmappedStatusError), as the update
// path already does before this runs.
func (s *Service) statusTargetFor(ctx context.Context, q *db.Queries, workspaceID, projectID pgtype.UUID, status string) (statusTarget, error) {
	m, err := loadStatusMapping(ctx, q, s.logger, workspaceID, projectID)
	if err != nil {
		return statusTarget{}, err
	}
	switch m.category(status) {
	case issuestatus.CategoryDone:
		return statusTarget{state: "closed", reason: "completed", mapping: m}, nil
	case issuestatus.CategoryClosed:
		return statusTarget{state: "closed", reason: "not_planned", mapping: m}, nil
	}
	if !m.mappable(status) {
		return statusTarget{}, &UnmappedStatusError{Status: status, Mappable: m.shown()}
	}
	target := statusTarget{state: "open", mapping: m}
	if status != m.entry {
		target.label = m.labels[status]
	}
	return target, nil
}

// BeginIssueUpdate sends the GitHub part of an issue update: title,
// description, and the state and status label a new status maps to. It
// returns nil when the issue is local or nothing GitHub holds changes. The
// caller then writes the local copy and calls Finish.
//
// The description of an issue opened on GitHub by someone else (the stored
// text carries the external prefix) is read-only.
func (s *Service) BeginIssueUpdate(ctx context.Context, actor Actor, issue db.Issue, change IssueChange) (*Write, error) {
	// A write that brings Multica in line with GitHub (status.go) must not
	// go back to GitHub.
	if s == nil || IsSyncedWrite(ctx) {
		return nil, nil
	}
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	ext, err := q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	setTitle := change.Title != nil && *change.Title != issue.Title
	setBody := change.Description != nil && *change.Description != issue.Description.String
	setStatus := change.Status != nil && *change.Status != issue.Status
	if setBody {
		if _, external := StripExternalIssuePrefix(issue.Description.String); external {
			return nil, &WriteError{Kind: WriteNotAllowed, msg: "the description of an issue opened on GitHub by someone else is read-only"}
		}
	}
	if !setTitle && !setBody && !setStatus {
		return nil, nil
	}

	var target statusTarget
	if setStatus {
		if target, err = s.statusTargetFor(ctx, q, issue.WorkspaceID, change.ProjectID, *change.Status); err != nil {
			return nil, err
		}
	}
	setState := setStatus && (target.state != ext.State || (target.state == "closed" && target.reason != ext.StateReason.String))
	repo, err := s.repoForIssue(ctx, ext)
	if err != nil {
		return nil, err
	}
	var labels []string
	if setStatus && len(target.mapping.labels) > 0 {
		current, err := s.readIssue(ctx, repo, ext.ExternalNumber)
		if err != nil {
			return nil, err
		}
		if next, changed := relabel(current.labelNames(), target); changed {
			labels = next
		}
	}
	if !setTitle && !setBody && !setState && labels == nil {
		return nil, nil
	}

	path := "/issues/" + strconv.Itoa(int(ext.ExternalNumber))
	intent, resp, err := s.send(ctx, writeRequest{
		op: opUpdateIssue, actor: actor, repo: repo, method: http.MethodPatch, path: path,
		issueID: issue.ID, number: ext.ExternalNumber,
		payload: func(op pgtype.UUID) map[string]any {
			p := map[string]any{}
			if setTitle {
				p["title"] = *change.Title
			}
			if setBody {
				p["body"] = signedBody(*change.Description, actor, op)
			}
			if setState {
				p["state"] = target.state
				if target.state == "closed" {
					p["state_reason"] = target.reason
				}
			}
			if labels != nil {
				p["labels"] = labels
			}
			return p
		},
	})
	if err != nil {
		return nil, err
	}
	w := &Write{svc: s, intent: intent, repo: repo}
	var gh githubIssue
	if json.Unmarshal(resp.Body, &gh) == nil && gh.Number == ext.ExternalNumber {
		w.issue = &gh
		s.accept(ctx, &w.intent, gh.ID, gh.Number)
		// A poll page read before this write must not roll the issue back.
		if err := q.BumpIssueExternalUpdatedAt(context.WithoutCancel(ctx), db.BumpIssueExternalUpdatedAtParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			ExternalUpdatedAt: pgtype.Timestamptz{Time: gh.UpdatedAt, Valid: true},
		}); err != nil {
			s.logger.Warn("github write: record updated_at", "intent_id", intent.ID, "error", err)
		}
	} else {
		s.accept(ctx, &w.intent, 0, ext.ExternalNumber)
	}
	return w, nil
}

// readIssue reads one issue before a write. Nothing is written yet, so every
// failure only refuses the write.
func (s *Service) readIssue(ctx context.Context, repo *Repo, number int32) (githubIssue, error) {
	resp, err := repo.Do(ctx, http.MethodGet, "/issues/"+strconv.Itoa(int(number)), nil)
	var limited *ghsnapshot.RateLimitError
	switch {
	case errors.As(err, &limited):
		return githubIssue{}, &WriteError{Kind: WriteRateLimited, RetryAfter: limited.RetryAfter, msg: "GitHub rate limit reached; try again later"}
	case errors.Is(err, ErrTokenNotUsable):
		return githubIssue{}, &WriteError{Kind: WriteUnavailable, msg: "the GitHub token is not usable"}
	case err != nil:
		return githubIssue{}, &WriteError{Kind: WriteUnavailable, msg: "could not read the issue from GitHub"}
	}
	if resp.StatusCode != http.StatusOK {
		return githubIssue{}, &WriteError{Kind: WriteRejected, StatusCode: resp.StatusCode,
			msg: "GitHub refused to read the issue (HTTP " + strconv.Itoa(resp.StatusCode) + ")"}
	}
	var gh githubIssue
	if err := json.Unmarshal(resp.Body, &gh); err != nil {
		return githubIssue{}, &WriteError{Kind: WriteUnavailable, msg: "could not read the issue from GitHub"}
	}
	return gh, nil
}

func (g githubIssue) labelNames() []string {
	names := make([]string, 0, len(g.Labels))
	for _, l := range g.Labels {
		names = append(names, l.Name)
	}
	return names
}

// relabel returns current without the labels the project maps a status to
// and with target's label, sorted, and whether that differs from current.
// Labels compare without regard to case, as on GitHub.
func relabel(current []string, target statusTarget) ([]string, bool) {
	next := make([]string, 0, len(current)+1)
	for _, name := range current {
		if _, mapped := target.mapping.keyOfLabel[strings.ToLower(strings.TrimSpace(name))]; !mapped {
			next = append(next, name)
		}
	}
	if target.label != "" {
		next = append(next, target.label)
	}
	sort.Strings(next)
	sorted := append([]string(nil), current...)
	sort.Strings(sorted)
	return next, strings.Join(next, "\x00") != strings.Join(sorted, "\x00")
}

// BeginIssueCreate opens an issue on GitHub for a create in Multica. The
// caller then creates the local issue, passing InTx to run inside its
// transaction, or calls Abandon if the local create fails.
//
// GitHub opens every new issue, so a status that maps to closed is refused,
// and so is one the project's status mapping cannot show (UnmappedStatusError).
func (s *Service) BeginIssueCreate(ctx context.Context, workspaceID pgtype.UUID, actor Actor, in NewIssue) (*Write, error) {
	if s == nil {
		return nil, ErrFeatureOff
	}
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	owner, name, _ := strings.Cut(strings.TrimSpace(in.Repo), "/")
	target, err := s.statusTargetFor(ctx, q, workspaceID, in.ProjectID, in.Status)
	var unmapped *UnmappedStatusError
	if errors.As(err, &unmapped) {
		unmapped.Issue = strings.TrimSpace(in.Repo) + " (new issue)"
	}
	if err != nil {
		return nil, err
	}
	if target.state != "open" {
		return nil, &WriteError{Kind: WriteNotAllowed, msg: "a GitHub issue is opened in an open status; create it, then close it"}
	}
	repo, err := s.RepoClient(ctx, workspaceID, owner, name)
	if err != nil {
		return nil, repoError(err)
	}
	intent, resp, err := s.send(ctx, writeRequest{
		op: opCreateIssue, actor: actor, repo: repo, method: http.MethodPost, path: "/issues",
		payload: func(op pgtype.UUID) map[string]any {
			p := map[string]any{"title": in.Title, "body": signedBody(in.Description, actor, op)}
			if target.label != "" {
				p["labels"] = []string{target.label}
			}
			return p
		},
	})
	if err != nil {
		return nil, err
	}
	var gh githubIssue
	if err := json.Unmarshal(resp.Body, &gh); err != nil || gh.Number <= 0 || (gh.State != "open" && gh.State != "closed") {
		// GitHub created something this reply does not describe; the poll
		// finds it by its marker.
		s.settleIntent(context.WithoutCancel(ctx), intent, intentUnknown, "malformed create response")
		return nil, &WriteError{Kind: WriteUnknown, msg: unconfirmedMessage}
	}
	w := &Write{svc: s, intent: intent, repo: repo, issue: &gh}
	s.accept(ctx, &w.intent, gh.ID, gh.Number)
	return w, nil
}

// errIssueAlreadyImported means the poller healed this create before the
// local transaction committed; the caller's create rolls back and the healed
// issue stays.
var errIssueAlreadyImported = errors.New("githubissues: the GitHub issue was already imported")

// InTx links the new local issue to the GitHub issue inside the create
// transaction, so the issue never exists without its issue_external row (the
// poller would otherwise import the GitHub issue a second time).
func (w *Write) InTx(ctx context.Context, q *db.Queries, issue db.Issue) error {
	if w == nil || w.issue == nil {
		return nil
	}
	gh := w.issue
	if err := q.LockIssueDuplicateKey(ctx, issueLockKey(w.repo, gh.Number)); err != nil {
		return err
	}
	_, err := q.GetIssueExternalByNumber(ctx, db.GetIssueExternalByNumberParams{
		WorkspaceID: w.repo.workspaceID, Provider: issuekey.ProviderGitHub,
		ExternalRepoID: w.repo.id, ExternalNumber: gh.Number,
	})
	if err == nil {
		return errIssueAlreadyImported
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := q.UpsertIssueExternal(ctx, db.UpsertIssueExternalParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Provider: issuekey.ProviderGitHub,
		ExternalRepoID: w.repo.id, ExternalRepo: w.repo.FullName(), ExternalNumber: gh.Number,
		ExternalNodeID: gh.NodeID, State: gh.State, StateReason: optionalText(gh.StateReason),
		ExternalUpdatedAt: pgtype.Timestamptz{Time: gh.UpdatedAt, Valid: true},
	}); err != nil {
		return err
	}
	_, err = q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: w.intent.ID, WorkspaceID: w.intent.WorkspaceID, State: intentApplied, IssueID: issue.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Settled by the poller in the meantime; the lookup above already
		// ruled out a second import.
		return nil
	}
	return err
}

// Abandon records that the local create failed after GitHub opened the
// issue. The next poll imports it (docs/github-issues.md, "自愈").
func (w *Write) Abandon(ctx context.Context, cause error) {
	if w == nil || cause == nil {
		return
	}
	w.localFailed(ctx, cause)
}

// issueLockKey serializes every writer of one GitHub issue in a workspace.
func issueLockKey(repo *Repo, number int32) string {
	return "githubissues:issue:" + util.UUIDToString(repo.workspaceID) + ":" +
		strconv.FormatInt(repo.id, 10) + ":" + strconv.Itoa(int(number))
}
