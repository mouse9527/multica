// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package migrations

import (
	"context"
	"testing"
)

var crossReadUp = []string{
	"9028_cross_workspace_read.up.sql",
	"9029_cross_workspace_read_grant_lookup_idx.up.sql",
	"9030_cross_workspace_read_audit_target_idx.up.sql",
}

var crossReadDown = []string{
	"9030_cross_workspace_read_audit_target_idx.down.sql",
	"9029_cross_workspace_read_grant_lookup_idx.down.sql",
	"9028_cross_workspace_read.down.sql",
}

var crossReadRelations = []string{
	"cross_workspace_read_grant",
	"cross_workspace_read_audit",
	"idx_cross_workspace_read_grant_lookup",
	"idx_cross_workspace_read_audit_target",
}

// TestCrossWorkspaceReadMigrationRoundTrip: 9028–9030 only add their two
// tables and two indexes, are re-runnable, leave valid indexes, and roll back
// to nothing; a second up after the rollback succeeds.
func TestCrossWorkspaceReadMigrationRoundTrip(t *testing.T) {
	const schema = "cross_workspace_read_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)

	for round := 1; round <= 2; round++ {
		applyGithubIssueMigrations(t, ctx, pool, crossReadUp)
		applyGithubIssueMigrations(t, ctx, pool, crossReadUp)
		if got := sandboxRelationCount(t, ctx, pool, schema, crossReadRelations); got != len(crossReadRelations) {
			t.Fatalf("round %d up: %d of %d relations exist", round, got, len(crossReadRelations))
		}
		var valid int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = ANY($2::text[]) AND i.indisvalid
		`, schema, crossReadRelations[2:]).Scan(&valid); err != nil {
			t.Fatalf("count valid indexes: %v", err)
		}
		if valid != 2 {
			t.Fatalf("round %d up: %d of 2 indexes valid", round, valid)
		}

		// The table's checks hold: a workspace cannot grant itself, and a
		// revoked grant always says who revoked it.
		if _, err := pool.Exec(ctx, `
			INSERT INTO cross_workspace_read_grant (target_workspace_id, source_workspace_id, created_by)
			VALUES ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', gen_random_uuid())`); err == nil {
			t.Fatalf("round %d: a grant from a workspace to itself was accepted", round)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO cross_workspace_read_grant (target_workspace_id, source_workspace_id, created_by, revoked_at)
			VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), now())`); err == nil {
			t.Fatalf("round %d: a revoked grant without revoked_by was accepted", round)
		}

		applyGithubIssueMigrations(t, ctx, pool, crossReadDown)
		applyGithubIssueMigrations(t, ctx, pool, crossReadDown)
		if got := sandboxRelationCount(t, ctx, pool, schema, crossReadRelations); got != 0 {
			t.Fatalf("round %d down: %d relations left behind", round, got)
		}
	}
}
