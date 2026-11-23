// Added by the mouse9527/multica fork: Evolution P2 (M-136), P4 (M-138).

package migrations

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var evolutionTables = []string{"evolution_cursor", "evolution_run", "evolution_proposal", "evolution_config_version"}

// TestEvolutionTablesMigrationRoundTrip: tables on up, re-runnable up,
// nothing left on down, and up again after a rollback.
func TestEvolutionTablesMigrationRoundTrip(t *testing.T) {
	const schema = "evolution_tables_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)

	up := []string{"9019_evolution_tables.up.sql", "9020_evolution_apply.up.sql"}
	down := []string{"9020_evolution_apply.down.sql", "9019_evolution_tables.down.sql"}
	for round := 1; round <= 2; round++ {
		applyGithubIssueMigrations(t, ctx, pool, up)
		if got := sandboxRelationCount(t, ctx, pool, schema, evolutionTables); got != len(evolutionTables) {
			t.Fatalf("round %d up: %d of %d tables exist", round, got, len(evolutionTables))
		}
		applyGithubIssueMigrations(t, ctx, pool, up)
		// Rolling back P4 alone leaves P2's tables in place.
		applyGithubIssueMigrations(t, ctx, pool, down[:1])
		if got := sandboxRelationCount(t, ctx, pool, schema, evolutionTables[:2]); got != 2 {
			t.Fatalf("round %d: rolling back 9020 removed P2 tables (%d of 2 left)", round, got)
		}
		if got := sandboxRelationCount(t, ctx, pool, schema, evolutionTables[2:]); got != 0 {
			t.Fatalf("round %d: rolling back 9020 left %d P4 tables", round, got)
		}
		applyGithubIssueMigrations(t, ctx, pool, down[1:])
		if got := sandboxRelationCount(t, ctx, pool, schema, evolutionTables); got != 0 {
			t.Fatalf("round %d down: %d tables left behind", round, got)
		}
	}
}

// TestEvolutionTablesStayInvisibleToSqlc pins why the Evolution migrations
// run their DDL inside DO blocks, for the reason given on
// TestRouterTablesStayInvisibleToSqlc: a table sqlc can see adds structs to
// the shared pkg/db/generated/models.go that every upstream sync touches.
func TestEvolutionTablesStayInvisibleToSqlc(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(realMigrationsDir(t), "*_evolution_*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Evolution migrations found: %v", err)
	}
	doBlock := regexp.MustCompile(`(?s)DO \$[a-z_]+\$.*?END\s*\$[a-z_]+\$;`)
	created := 0
	for _, file := range files {
		src := readMigrationFile(t, filepath.Base(file))
		for _, block := range doBlock.FindAllString(src, -1) {
			created += strings.Count(block, "CREATE TABLE IF NOT EXISTS ")
		}
		outside := strings.ToUpper(stripSQLComments(doBlock.ReplaceAllString(src, "")))
		for _, ddl := range []string{"CREATE TABLE", "ALTER TABLE"} {
			if strings.Contains(outside, ddl) {
				t.Errorf("%s has %s outside a DO block; sqlc would see it", filepath.Base(file), ddl)
			}
		}
	}
	if created != len(evolutionTables) {
		t.Fatalf("DO blocks create %d Evolution tables, want %d", created, len(evolutionTables))
	}
}
