// Added by the mouse9527/multica fork: Evolution P3 (M-137), shared with
// apply and rollback in P4 (M-138).

package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrTargetNotFound means the target is not in the workspace.
var ErrTargetNotFound = errors.New("evolution: target not found")

// TargetState is what decides whether a target may be proposed or changed,
// beyond its whitelisted fields.
type TargetState struct {
	Kind      string // agent kind
	SystemKey string // agent system_key
	Archived  bool
	// IsDefault marks the workspace's Default workflow, whose name is fixed.
	IsDefault bool
}

// ReadTarget reads a target's whitelisted fields, the snapshot a proposal's
// base hash and every version row are taken of. With lock, the agent or
// workflow row is locked for the rest of the caller's transaction; squads
// are never written, so they are never locked.
func ReadTarget(ctx context.Context, q Querier, workspaceID, targetType, targetID string, lock bool) (TargetSnapshot, TargetState, error) {
	snap := TargetSnapshot{Type: targetType, ID: targetID, Fields: map[string]string{}}
	var st TargetState
	forUpdate := ""
	if lock {
		forUpdate = " FOR NO KEY UPDATE"
	}
	switch targetType {
	case TargetAgent:
		var instructions, description string
		err := q.QueryRow(ctx, `
			SELECT name, description, instructions, kind, COALESCE(system_key, ''), archived_at IS NOT NULL
			FROM agent WHERE id = $1::uuid AND workspace_id = $2::uuid`+forUpdate, targetID, workspaceID).
			Scan(&snap.Name, &description, &instructions, &st.Kind, &st.SystemKey, &st.Archived)
		if errors.Is(err, pgx.ErrNoRows) {
			return snap, st, ErrTargetNotFound
		}
		if err != nil {
			return snap, st, err
		}
		skills, err := queryLines(ctx, q, `
			SELECT s.name FROM agent_skill x JOIN skill s ON s.id = x.skill_id
			WHERE x.agent_id = $1::uuid AND x.enabled ORDER BY lower(s.name), s.name`, targetID)
		if err != nil {
			return snap, st, err
		}
		snap.Fields["instructions"], snap.Fields["description"], snap.Fields["skills"] = instructions, description, skills
	case TargetWorkflow:
		var description string
		var steps []byte
		err := q.QueryRow(ctx, `
			SELECT name, description, steps, is_default FROM issue_workflow WHERE id = $1::uuid AND workspace_id = $2::uuid`+forUpdate,
			targetID, workspaceID).Scan(&snap.Name, &description, &steps, &st.IsDefault)
		if errors.Is(err, pgx.ErrNoRows) {
			return snap, st, ErrTargetNotFound
		}
		if err != nil {
			return snap, st, err
		}
		snap.Fields["name"], snap.Fields["description"], snap.Fields["steps"] = snap.Name, description, PrettySteps(steps)
	case TargetSquad:
		var description, instructions, leaderID, leaderName string
		err := q.QueryRow(ctx, `
			SELECT s.name, s.description, s.instructions, COALESCE(s.leader_id::text, ''), COALESCE(l.name, ''), s.archived_at IS NOT NULL
			FROM squad s LEFT JOIN agent l ON l.id = s.leader_id
			WHERE s.id = $1::uuid AND s.workspace_id = $2::uuid`, targetID, workspaceID).
			Scan(&snap.Name, &description, &instructions, &leaderID, &leaderName, &st.Archived)
		if errors.Is(err, pgx.ErrNoRows) {
			return snap, st, ErrTargetNotFound
		}
		if err != nil {
			return snap, st, err
		}
		// Members are listed by agent name, people by id: proposals never
		// name members.
		members, err := queryLines(ctx, q, `
			SELECT CASE m.member_type WHEN 'agent' THEN 'agent · ' || COALESCE(a.name, m.member_id::text)
			                          ELSE m.member_type || ' · ' || m.member_id::text END
			       || CASE WHEN COALESCE(m.role, '') = '' THEN '' ELSE ' · ' || m.role END
			FROM squad_member m LEFT JOIN agent a ON m.member_type = 'agent' AND a.id = m.member_id
			WHERE m.squad_id = $1::uuid
			ORDER BY m.member_type, lower(COALESCE(a.name, '')), m.member_id`, targetID)
		if err != nil {
			return snap, st, err
		}
		snap.Fields["description"], snap.Fields["instructions"] = description, instructions
		snap.Fields["members"], snap.Fields["leader"] = members, leaderName+" · "+leaderID
	default:
		return snap, st, ErrTargetNotFound
	}
	return snap, st, nil
}

// PrettySteps is a workflow's stored steps as the snapshot shows them:
// indented JSON with sorted keys. Text that is not JSON shows as nothing.
func PrettySteps(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}

// SkillNames splits a snapshot's skills field into names, one per line,
// skipping blank lines.
func SkillNames(field string) []string {
	out := []string{}
	for _, line := range strings.Split(field, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func queryLines(ctx context.Context, q Querier, sql string, args ...any) (string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), rows.Err()
}
