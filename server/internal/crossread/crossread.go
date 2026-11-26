// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

// Package crossread stores cross-workspace read grants and their audit trail.
//
// A grant lets the agents of a source workspace (one agent, or all of them)
// read a target workspace through their own task tokens. The target's
// owner/admin creates and revokes it; the middleware asks for the live grant
// on every request, so revoking or expiring takes effect on the next request.
//
// All SQL is hand-written against tables sqlc does not see (migration 9028).
package crossread

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// MaxAuditPage caps one audit listing.
const MaxAuditPage = 500

// maxRouteLen matches the route column's check.
const maxRouteLen = 512

// Querier is the slice of a pgx pool or transaction the store uses.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Grant is one row of cross_workspace_read_grant. IDs are canonical UUID
// strings; AgentID is empty when the grant covers every agent of the source.
type Grant struct {
	ID                string     `json:"id"`
	TargetWorkspaceID string     `json:"target_workspace_id"`
	SourceWorkspaceID string     `json:"source_workspace_id"`
	AgentID           string     `json:"agent_id,omitempty"`
	Note              string     `json:"note"`
	CreatedBy         string     `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
	RevokedAt         *time.Time `json:"revoked_at"`
	RevokedBy         string     `json:"revoked_by,omitempty"`
}

// AuditEntry is what the middleware records for one cross-workspace request.
type AuditEntry struct {
	GrantID           string `json:"grant_id"`
	TargetWorkspaceID string `json:"target_workspace_id"`
	SourceWorkspaceID string `json:"source_workspace_id"`
	AgentID           string `json:"agent_id"`
	TaskID            string `json:"task_id"`
	Method            string `json:"method"`
	Route             string `json:"route"`
	Allowed           bool   `json:"allowed"`
}

// AuditRow is a stored AuditEntry.
type AuditRow struct {
	ID string `json:"id"`
	AuditEntry
	CreatedAt time.Time `json:"created_at"`
}

// CreateGrantParams is a new grant. AgentID empty means every agent of the
// source workspace; ExpiresAt nil means until revoked.
type CreateGrantParams struct {
	TargetWorkspaceID string
	SourceWorkspaceID string
	AgentID           string
	Note              string
	CreatedBy         string
	ExpiresAt         *time.Time
}

// Store reads and writes the grant and audit tables.
type Store struct {
	db    Querier
	flags *featureflag.Service
}

func NewStore(db Querier, flags *featureflag.Service) *Store {
	return &Store{db: db, flags: flags}
}

// Enabled reports whether the cross_workspace_read flag is on.
func (s *Store) Enabled(ctx context.Context) bool {
	return featureflags.CrossWorkspaceReadEnabled(ctx, s.flags)
}

const grantColumns = `id::text, target_workspace_id::text, source_workspace_id::text,
	COALESCE(agent_id::text, ''), note, created_by::text, created_at, expires_at,
	revoked_at, COALESCE(revoked_by::text, '')`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	err := row.Scan(&g.ID, &g.TargetWorkspaceID, &g.SourceWorkspaceID, &g.AgentID, &g.Note,
		&g.CreatedBy, &g.CreatedAt, &g.ExpiresAt, &g.RevokedAt, &g.RevokedBy)
	return g, err
}

// ActiveGrant returns the live grant that lets agentID of sourceWorkspaceID
// read targetWorkspaceID: not revoked, not expired (by the database clock),
// naming the agent or every agent. A grant naming the agent wins over one for
// every agent, so the audit points at the most specific permission.
func (s *Store) ActiveGrant(ctx context.Context, targetWorkspaceID, sourceWorkspaceID, agentID string) (Grant, bool, error) {
	g, err := scanGrant(s.db.QueryRow(ctx, `
		SELECT `+grantColumns+`
		FROM cross_workspace_read_grant
		WHERE target_workspace_id = $1::uuid
		  AND source_workspace_id = $2::uuid
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (agent_id IS NULL OR agent_id = $3::uuid)
		ORDER BY (agent_id IS NULL), created_at
		LIMIT 1`, targetWorkspaceID, sourceWorkspaceID, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, err
	}
	return g, true, nil
}

// RecordAudit writes one audit row. The middleware serves the request only
// after this returns nil.
func (s *Store) RecordAudit(ctx context.Context, e AuditEntry) error {
	route := auditRoute(e.Route)
	_, err := s.db.Exec(ctx, `
		INSERT INTO cross_workspace_read_audit
			(grant_id, target_workspace_id, source_workspace_id, agent_id, task_id, method, route, allowed)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6, $7, $8)`,
		e.GrantID, e.TargetWorkspaceID, e.SourceWorkspaceID, e.AgentID, e.TaskID, e.Method, route, e.Allowed)
	return err
}

// auditRoute makes a route storable: a refused request's path is decoded
// from the URL and may hold invalid UTF-8, a NUL, or be longer than the column
// allows. A route Postgres rejects would fail the audit write, turning the
// refusal into a 503 with no audit row, so it is repaired and cut on a rune
// boundary instead.
func auditRoute(route string) string {
	route = strings.ReplaceAll(strings.ToValidUTF8(route, "\uFFFD"), "\x00", "\uFFFD")
	if len(route) <= maxRouteLen {
		return route
	}
	route = route[:maxRouteLen]
	for !utf8.ValidString(route) {
		route = route[:len(route)-1]
	}
	return route
}

// CreateGrant inserts a grant. The caller has checked who may grant and that
// the source workspace and agent exist.
func (s *Store) CreateGrant(ctx context.Context, p CreateGrantParams) (Grant, error) {
	var agent any
	if p.AgentID != "" {
		agent = p.AgentID
	}
	return scanGrant(s.db.QueryRow(ctx, `
		INSERT INTO cross_workspace_read_grant
			(target_workspace_id, source_workspace_id, agent_id, note, created_by, expires_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, $6)
		RETURNING `+grantColumns,
		p.TargetWorkspaceID, p.SourceWorkspaceID, agent, p.Note, p.CreatedBy, p.ExpiresAt))
}

// ListGrants returns every grant of a target workspace, revoked and expired
// ones included, newest first.
func (s *Store) ListGrants(ctx context.Context, targetWorkspaceID string) ([]Grant, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+grantColumns+`
		FROM cross_workspace_read_grant
		WHERE target_workspace_id = $1::uuid
		ORDER BY created_at DESC`, targetWorkspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// RevokeGrant revokes a grant of the target workspace. Revoking an already
// revoked grant keeps the first revocation and still returns the grant; found
// is false only when the target has no such grant.
func (s *Store) RevokeGrant(ctx context.Context, targetWorkspaceID, grantID, revokedBy string) (Grant, bool, error) {
	if _, err := s.db.Exec(ctx, `
		UPDATE cross_workspace_read_grant
		SET revoked_at = now(), revoked_by = $3::uuid
		WHERE id = $1::uuid AND target_workspace_id = $2::uuid AND revoked_at IS NULL`,
		grantID, targetWorkspaceID, revokedBy); err != nil {
		return Grant{}, false, err
	}
	g, err := scanGrant(s.db.QueryRow(ctx, `
		SELECT `+grantColumns+`
		FROM cross_workspace_read_grant
		WHERE id = $1::uuid AND target_workspace_id = $2::uuid`, grantID, targetWorkspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, err
	}
	return g, true, nil
}

// ListAudit returns a target workspace's newest audit rows, at most limit
// (clamped to 1..MaxAuditPage).
func (s *Store) ListAudit(ctx context.Context, targetWorkspaceID string, limit int) ([]AuditRow, error) {
	if limit <= 0 || limit > MaxAuditPage {
		limit = MaxAuditPage
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, grant_id::text, target_workspace_id::text, source_workspace_id::text,
			agent_id::text, task_id::text, method, route, allowed, created_at
		FROM cross_workspace_read_audit
		WHERE target_workspace_id = $1::uuid
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, targetWorkspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var a AuditRow
		if err := rows.Scan(&a.ID, &a.GrantID, &a.TargetWorkspaceID, &a.SourceWorkspaceID,
			&a.AgentID, &a.TaskID, &a.Method, &a.Route, &a.Allowed, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteWorkspaceData removes the grants a workspace gave or received and
// their audit rows, inside the workspace teardown transaction.
func DeleteWorkspaceData(ctx context.Context, q Querier, workspaceID any) error {
	for _, table := range []string{"cross_workspace_read_audit", "cross_workspace_read_grant"} {
		if _, err := q.Exec(ctx, `DELETE FROM `+table+`
			WHERE target_workspace_id = $1 OR source_workspace_id = $1`, workspaceID); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return nil
}
