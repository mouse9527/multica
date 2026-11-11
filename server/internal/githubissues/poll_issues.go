package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// githubIssue is the part of a GitHub issue the poller reads.
type githubIssue struct {
	NodeID      string        `json:"node_id"`
	Number      int32         `json:"number"`
	Title       string        `json:"title"`
	Body        *string       `json:"body"`
	State       string        `json:"state"`
	StateReason *string       `json:"state_reason"`
	UpdatedAt   time.Time     `json:"updated_at"`
	Labels      []githubLabel `json:"labels"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
	// PullRequest is present when the item is a pull request: the issues
	// list returns both.
	PullRequest json.RawMessage `json:"pull_request"`
}

func (g githubIssue) body() string {
	if g.Body == nil {
		return ""
	}
	return *g.Body
}

// pollIssues reads the issues stream of a repository from cursor and stores
// every issue in it. Pull requests are skipped.
func (s *Service) pollIssues(ctx context.Context, repo *Repo, projectID pgtype.UUID, cursor pgtype.Timestamptz) (synced int, caughtUp bool, err error) {
	caughtUp, err = s.readStream(ctx, repo, stream{path: "/issues"}, cursor, func(items []json.RawMessage) (int, error) {
		for i, raw := range items {
			var issue githubIssue
			if err := json.Unmarshal(raw, &issue); err != nil {
				return i, errors.New("github GET /issues: malformed issue")
			}
			if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
				continue
			}
			changed, err := s.syncIssue(ctx, repo, projectID, issue)
			if err != nil {
				return i, fmt.Errorf("sync %s#%d: %w", repo.FullName(), issue.Number, err)
			}
			if changed {
				synced++
			}
		}
		return len(items), nil
	})
	return synced, caughtUp, err
}

// syncIssue stores one GitHub issue: a new local issue the first time it is
// seen, otherwise a refresh of the issue that mirrors it. A snapshot older
// than the one already stored (external_updated_at) changes nothing.
func (s *Service) syncIssue(ctx context.Context, repo *Repo, projectID pgtype.UUID, gh githubIssue) (bool, error) {
	if gh.Number <= 0 || (gh.State != "open" && gh.State != "closed") {
		return false, errors.New("issue without number or state")
	}
	// A collision on the workspace issue number means the counter trails
	// MAX(number) (see service.IssueService.Create); heal it and run the
	// transaction once more.
	for attempt := 0; ; attempt++ {
		created, changed, current, err := s.syncIssueOnce(ctx, repo, projectID, gh)
		var pgErr *pgconn.PgError
		if attempt == 0 && errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == service.IssueNumberConstraint {
			if _, healErr := db.New(s.db).HealIssueCounter(ctx, repo.workspaceID); healErr != nil && !errors.Is(healErr, pgx.ErrNoRows) {
				return false, healErr
			}
			continue
		}
		if err != nil {
			return false, err
		}
		if created != nil {
			s.publishIssueCreated(ctx, *created)
			return true, nil
		}
		if changed != nil {
			s.publishIssueChanged(*changed)
		}
		// Committed first: the status write runs the issue write path, which
		// has a transaction of its own.
		statusChanged := false
		if current.ID.Valid {
			if statusChanged, err = s.syncStatus(ctx, repo, current, gh); err != nil {
				return changed != nil, fmt.Errorf("sync status: %w", err)
			}
		}
		return changed != nil || statusChanged, nil
	}
}

// current is the stored issue after the transaction when it existed before,
// for the status sync that follows; it is zero when the snapshot was stale.
func (s *Service) syncIssueOnce(ctx context.Context, repo *Repo, projectID pgtype.UUID, gh githubIssue) (created, changed *db.Issue, current db.Issue, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, db.Issue{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)

	// Serializes every writer of this GitHub issue in the workspace, so two
	// syncs (a poll and a later on-demand refresh) never both create it.
	if err := q.LockIssueDuplicateKey(ctx, "githubissues:issue:"+util.UUIDToString(repo.workspaceID)+":"+
		strconv.FormatInt(repo.id, 10)+":"+strconv.Itoa(int(gh.Number))); err != nil {
		return nil, nil, db.Issue{}, err
	}
	existing, err := q.GetIssueExternalByNumber(ctx, db.GetIssueExternalByNumberParams{
		WorkspaceID: repo.workspaceID, Provider: issuekey.ProviderGitHub,
		ExternalRepoID: repo.id, ExternalNumber: gh.Number,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		issue, err := s.createIssue(ctx, tx, q, repo, projectID, gh)
		if err != nil {
			return nil, nil, db.Issue{}, err
		}
		created = &issue
	case err != nil:
		return nil, nil, db.Issue{}, err
	default:
		if gh.UpdatedAt.Before(existing.ExternalUpdatedAt.Time) {
			return nil, nil, db.Issue{}, nil
		}
		issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: existing.IssueID, WorkspaceID: repo.workspaceID})
		if err != nil {
			return nil, nil, db.Issue{}, err
		}
		params := db.UpdateGitHubIssueContentParams{
			ID: issue.ID, WorkspaceID: repo.workspaceID,
			Title: NormalizeGitHubText(gh.Title), Description: issue.Description,
		}
		// Only a description written by the poller carries the external
		// prefix. One written from Multica is left to the write path.
		if _, external := StripExternalIssuePrefix(issue.Description.String); external {
			params.Description = pgtype.Text{String: ExternalIssueDescription(gh.User.Login, gh.body()), Valid: true}
		}
		current = issue
		updated, err := q.UpdateGitHubIssueContent(ctx, params)
		switch {
		case err == nil:
			changed = &updated
			current = updated
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, nil, db.Issue{}, err
		}
		if existing.State != gh.State || existing.StateReason.String != stringValue(gh.StateReason) {
			if changed == nil {
				changed = &issue
			}
		}
	}

	if _, err := q.UpsertIssueExternal(ctx, db.UpsertIssueExternalParams{
		IssueID: issueID(created, changed, existing), WorkspaceID: repo.workspaceID,
		Provider: issuekey.ProviderGitHub, ExternalRepoID: repo.id, ExternalRepo: repo.FullName(),
		ExternalNumber: gh.Number, ExternalNodeID: gh.NodeID,
		State: gh.State, StateReason: optionalText(gh.StateReason),
		ExternalUpdatedAt: pgtype.Timestamptz{Time: gh.UpdatedAt, Valid: true},
	}); err != nil {
		return nil, nil, db.Issue{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, db.Issue{}, err
	}
	return created, changed, current, nil
}

func issueID(created, changed *db.Issue, existing db.IssueExternal) pgtype.UUID {
	switch {
	case created != nil:
		return created.ID
	case changed != nil:
		return changed.ID
	default:
		return existing.IssueID
	}
}

// createIssue inserts the local issue for a GitHub issue opened outside
// Multica. The creator is the sync account (IssueCreator), the GitHub author
// is named in the description prefix, and nobody is assigned, so no agent run
// starts.
func (s *Service) createIssue(ctx context.Context, tx pgx.Tx, q *db.Queries, repo *Repo, projectID pgtype.UUID, gh githubIssue) (db.Issue, error) {
	creator, err := s.IssueCreator(ctx, repo.workspaceID)
	if err != nil {
		return db.Issue{}, err
	}
	status, err := initialStatus(ctx, q, s.logger, repo.workspaceID, projectID, gh)
	if err != nil {
		return db.Issue{}, err
	}
	// Self-hosted deployments have no issue-count entitlement.
	number, err := service.AllocateIssueNumber(ctx, q, repo.workspaceID, service.IssueCountPolicy{Action: entitlement.ActionOff})
	if err != nil {
		return db.Issue{}, fmt.Errorf("allocate issue number: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, repo.workspaceID, status)
	if err != nil {
		return db.Issue{}, err
	}
	return q.CreateIssue(ctx, db.CreateIssueParams{
		ID:          dbid.NewV7(),
		WorkspaceID: repo.workspaceID,
		Title:       NormalizeGitHubText(gh.Title),
		Description: pgtype.Text{String: ExternalIssueDescription(gh.User.Login, gh.body()), Valid: true},
		Status:      status,
		Priority:    "none",
		CreatorType: creator.Type,
		CreatorID:   creator.ID,
		Position:    position,
		Number:      number,
		ProjectID:   projectID,
	})
}

// initialStatus is the status a GitHub issue starts on in Multica: the one
// its state and status labels map to (statusMapping.fromGitHub). Later state
// changes go through syncStatus. A project workflow that does not list the
// status starts the issue on its first step, as every other create path does.
func initialStatus(ctx context.Context, q *db.Queries, logger *slog.Logger, workspaceID, projectID pgtype.UUID, gh githubIssue) (string, error) {
	m, err := loadStatusMapping(ctx, q, logger, workspaceID, projectID)
	if err != nil {
		return "", err
	}
	status, _ := m.fromGitHub(gh.State, stringValue(gh.StateReason), gh.Labels, "")
	var def *issueworkflow.Definition
	if projectID.Valid {
		def, _, err = issueworkflow.ForProject(ctx, q, workspaceID, projectID)
	} else {
		def, err = issueworkflow.OutsideProject(ctx, q, workspaceID)
	}
	if err != nil {
		return "", err
	}
	if def != nil && def.Limits() && !def.Has(status) {
		status = issueworkflow.StartStatus
	}
	return status, nil
}

// publishIssueCreated broadcasts a new issue so open boards show it. The
// payload carries no description: the subscriber listener reads mention links
// out of an issue:created description, and text from GitHub must not
// subscribe or notify anyone. Clients load the description with the issue.
func (s *Service) publishIssueCreated(ctx context.Context, issue db.Issue) {
	if s.bus == nil {
		return
	}
	q := db.New(s.db)
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		s.logger.Warn("github issues poll: load workspace for event", "error", err)
		return
	}
	payload := service.IssueToMapResolved(ctx, q, issue, workspace.IssuePrefix)
	payload["description"] = nil
	s.bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   externalAuthorType,
		Payload:     map[string]any{"issue": payload},
	})
}

// publishIssueChanged tells clients to reload the issue. The payload is only
// the id and carries no revision, which makes clients refetch it instead of
// patching their cache; the listeners that act on issue content need a full
// issue and skip it, so GitHub text again reaches no mention parser.
func (s *Service) publishIssueChanged(issue db.Issue) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   externalAuthorType,
		Payload: map[string]any{"issue": map[string]any{
			"id":           util.UUIDToString(issue.ID),
			"workspace_id": util.UUIDToString(issue.WorkspaceID),
		}},
	})
}

func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optionalText(s *string) pgtype.Text {
	if s == nil || *s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
