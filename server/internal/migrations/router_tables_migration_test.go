// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package migrations

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var routerTables = []string{"router_subscription", "router_event", "router_issue_seen", "router_reconcile_cursor"}

var routerIndexes = []string{
	"idx_router_subscription_scope",
	"idx_router_event_key",
	"idx_router_event_subscription",
}

var routerUpMigrations = []string{
	"9004_router_tables.up.sql",
	"9005_router_subscription_scope_idx.up.sql",
	"9006_router_event_key_idx.up.sql",
	"9007_router_event_subscription_idx.up.sql",
}

var routerDownMigrations = []string{
	"9007_router_event_subscription_idx.down.sql",
	"9006_router_event_key_idx.down.sql",
	"9005_router_subscription_scope_idx.down.sql",
	"9004_router_tables.down.sql",
}

// TestRouterTablesMigrationRoundTrip covers the up/down contract of the Router
// migrations: tables and valid indexes on up, re-runnable up, nothing left on
// down, and up again after a rollback.
func TestRouterTablesMigrationRoundTrip(t *testing.T) {
	const schema = "router_tables_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)

	for round := 1; round <= 2; round++ {
		applyGithubIssueMigrations(t, ctx, pool, routerUpMigrations)
		if got := sandboxRelationCount(t, ctx, pool, schema, routerTables); got != len(routerTables) {
			t.Fatalf("round %d up: %d of %d tables exist", round, got, len(routerTables))
		}
		var valid int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = ANY($2::text[]) AND i.indisvalid
		`, schema, routerIndexes).Scan(&valid); err != nil {
			t.Fatalf("count valid indexes: %v", err)
		}
		if valid != len(routerIndexes) {
			t.Fatalf("round %d up: %d of %d indexes valid", round, valid, len(routerIndexes))
		}

		applyGithubIssueMigrations(t, ctx, pool, routerUpMigrations)

		applyGithubIssueMigrations(t, ctx, pool, routerDownMigrations)
		all := append(append([]string{}, routerTables...), routerIndexes...)
		if got := sandboxRelationCount(t, ctx, pool, schema, all); got != 0 {
			t.Fatalf("round %d down: %d relations left behind", round, got)
		}
	}
}

// TestRouterEventKeyIsUniquePerAgent pins the idempotency contract the
// listener and reconcile rely on: one row per (agent, event_key), while two
// agents may each hold the same key.
func TestRouterEventKeyIsUniquePerAgent(t *testing.T) {
	const schema = "router_event_key_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, routerUpMigrations)

	insert := `
		INSERT INTO router_event (workspace_id, subscription_id, agent_id, event_key, kind, issue_id, source, occurred_at)
		VALUES (gen_random_uuid(), gen_random_uuid(), $1, 'comment:1', 'comment_created', gen_random_uuid(), 'bus', now())
		ON CONFLICT (agent_id, event_key) DO NOTHING`
	agentA := testUUID(t, "00000000-0000-0000-0000-00000000b001")
	agentB := testUUID(t, "00000000-0000-0000-0000-00000000b002")
	for _, agent := range []any{agentA, agentA, agentB} {
		if _, err := pool.Exec(ctx, insert, agent); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM router_event`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d rows, want one per agent", n)
	}
}

// TestRouterTablesStayInvisibleToSqlc pins why the Router migrations run
// their table DDL inside DO blocks: sqlc reads the migrations directory as its
// schema. A CREATE TABLE it can see adds Router structs to the shared
// pkg/db/generated/models.go, and an ALTER TABLE on a table it cannot see
// fails generation. The Router package uses hand-written SQL only, so every
// Router migration, including later slices', keeps both inside DO blocks.
// Index files may stay plain: CREATE INDEX CONCURRENTLY cannot run in a DO
// block, and sqlc ignores index statements.
func TestRouterTablesStayInvisibleToSqlc(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(realMigrationsDir(t), "*_router_*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Router migrations found: %v", err)
	}
	doBlock := regexp.MustCompile(`(?s)DO \$[a-z_]+\$.*?END\s*\$[a-z_]+\$;`)
	created := 0
	for _, file := range files {
		src := readMigrationFile(t, filepath.Base(file))
		for _, block := range doBlock.FindAllString(src, -1) {
			created += strings.Count(block, "CREATE TABLE IF NOT EXISTS router_")
		}
		outside := strings.ToUpper(stripSQLComments(doBlock.ReplaceAllString(src, "")))
		for _, ddl := range []string{"CREATE TABLE", "ALTER TABLE"} {
			if strings.Contains(outside, ddl) {
				t.Errorf("%s has %s outside a DO block; sqlc would see it", filepath.Base(file), ddl)
			}
		}
	}
	if created != len(routerTables) {
		t.Fatalf("DO blocks create %d Router tables, want %d", created, len(routerTables))
	}
}

func stripSQLComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
