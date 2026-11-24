// Added by the mouse9527/multica fork: the project Evolution files its issues
// into (M-231).

package evolution

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// projectUsable reports whether projectID is a project of the workspace that
// can hold Evolution's issues, and if not, why (a client-facing reason).
// Private projects are refused: a Super Agent subscribed to one cannot write
// into it, and Evolution never reads private projects.
func projectUsable(ctx context.Context, q Querier, workspaceID, projectID string) (string, error) {
	var private, archived bool
	err := q.QueryRow(ctx, `
		SELECT visibility = 'private', archived_at IS NOT NULL
		FROM project WHERE id = $1::uuid AND workspace_id = $2::uuid`,
		projectID, workspaceID).Scan(&private, &archived)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no such project in this workspace", nil
	case err != nil:
		return "", err
	case private:
		return "a private project cannot hold Evolution's issues", nil
	case archived:
		return "the project is archived", nil
	}
	return "", nil
}

// checkOutputProject rejects a configured output project that cannot be used.
func checkOutputProject(ctx context.Context, q Querier, workspaceID string, cfg Config) error {
	if cfg.OutputProjectID == "" {
		return nil
	}
	reason, err := projectUsable(ctx, q, workspaceID, cfg.OutputProjectID)
	if err != nil {
		return err
	}
	if reason != "" {
		return invalidf("output_project_id: %s", reason)
	}
	return nil
}

// outputProject is the project a new Evolution issue goes into: the
// configured one while it is still usable, otherwise none. A project deleted,
// archived, or made private after it was configured, or a failed lookup,
// files the issue without a project rather than failing the run.
func outputProject(ctx context.Context, q Querier, workspaceID string, cfg Config) pgtype.UUID {
	if cfg.OutputProjectID == "" {
		return pgtype.UUID{}
	}
	reason, err := projectUsable(ctx, q, workspaceID, cfg.OutputProjectID)
	if err != nil || reason != "" {
		slog.Warn("evolution: output project skipped", "workspace_id", workspaceID,
			"project_id", cfg.OutputProjectID, "reason", reason, "error", err)
		return pgtype.UUID{}
	}
	id, err := util.ParseUUID(cfg.OutputProjectID)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}
