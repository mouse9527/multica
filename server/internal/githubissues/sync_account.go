package githubissues

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrSyncAccountUnavailable means no account can be recorded as the creator
// of an issue opened on GitHub by someone outside the workspace: the
// configured account is not in the workspace, or the workspace has no owner.
var ErrSyncAccountUnavailable = errors.New("github sync account unavailable in workspace")

// SyncAccount is the Multica account recorded as the creator of issues opened
// on GitHub by people outside the workspace. issue.creator_type only admits
// member and agent, and existing table constraints are not changed, so the
// creator is a real account; the GitHub author is named in the description
// prefix instead (ExternalIssueDescription).
type SyncAccount struct {
	// Type is "member" or "agent", matching issue.creator_type.
	Type string
	// ID is the user id for a member and the agent id for an agent, matching
	// issue.creator_id.
	ID pgtype.UUID
}

func parseSyncAccount(raw string) (SyncAccount, bool) {
	kind, id, found := strings.Cut(raw, ":")
	if !found || (kind != "member" && kind != "agent") {
		return SyncAccount{}, false
	}
	parsed, err := util.ParseUUID(strings.TrimSpace(id))
	if err != nil {
		return SyncAccount{}, false
	}
	return SyncAccount{Type: kind, ID: parsed}, true
}

// IssueCreator returns the creator of an issue opened on GitHub by someone
// outside the workspace: the configured sync account when it belongs to the
// workspace, or the workspace's earliest owner when none is configured. A
// configured account that does not belong to the workspace is an error rather
// than a silent fallback, so a misconfiguration shows up in the sync status.
func (s *Service) IssueCreator(ctx context.Context, workspaceID pgtype.UUID) (SyncAccount, error) {
	q, err := s.queries()
	if err != nil {
		return SyncAccount{}, err
	}
	account := s.cfg.syncAccount
	switch account.Type {
	case "member":
		_, err = q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID: account.ID, WorkspaceID: workspaceID,
		})
	case "agent":
		var agent db.Agent
		agent, err = q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID: account.ID, WorkspaceID: workspaceID,
		})
		if err == nil && agent.ArchivedAt.Valid {
			err = pgx.ErrNoRows
		}
	default:
		account = SyncAccount{Type: "member"}
		account.ID, err = q.GetGitHubSyncDefaultOwner(ctx, workspaceID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncAccount{}, fmt.Errorf("%w (%s)", ErrSyncAccountUnavailable, EnvSyncAccount)
	}
	if err != nil {
		return SyncAccount{}, err
	}
	return account, nil
}
