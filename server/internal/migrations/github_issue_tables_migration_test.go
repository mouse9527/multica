// Modified for the mouse9527/multica fork: GitHub-native issues.

package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var githubIssueTables = []string{
	"issue_external",
	"github_repo_sync",
	"github_write_intent",
	"project_github_settings",
	"comment_external",
}

var githubIssueIndexes = []string{
	"idx_issue_external_number",
	"idx_comment_external_external_id",
	"idx_github_write_intent_open",
}

var githubIssueUpMigrations = []string{
	"9000_githubissues_tables.up.sql",
	"9001_githubissues_issue_external_number_idx.up.sql",
	"9002_githubissues_comment_external_id_idx.up.sql",
	"9003_githubissues_write_intent_open_idx.up.sql",
}

var githubIssueDownMigrations = []string{
	"9003_githubissues_write_intent_open_idx.down.sql",
	"9002_githubissues_comment_external_id_idx.down.sql",
	"9001_githubissues_issue_external_number_idx.down.sql",
	"9000_githubissues_tables.down.sql",
}

// newGithubIssueSandbox returns a pool whose search_path is an empty schema,
// so the migrations under test create their objects there and nowhere else.
func newGithubIssueSandbox(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	}
	cleanup()
	t.Cleanup(func() {
		cleanup()
		pool.Close()
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create isolated migration schema: %v", err)
	}
	return pool
}

func applyGithubIssueMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, files []string) {
	t.Helper()
	for _, name := range files {
		applyMigrationFile(t, ctx, pool, name)
	}
}

func sandboxRelationCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string, names []string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = ANY($2::text[])
	`, schema, names).Scan(&n); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	return n
}

// TestGithubIssueTablesMigrationRoundTrip covers the up/down contract: every
// table and index appears on up, the indexes are valid, everything is gone on
// down, and a second up after rollback succeeds.
func TestGithubIssueTablesMigrationRoundTrip(t *testing.T) {
	const schema = "github_issue_tables_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)

	for round := 1; round <= 2; round++ {
		applyGithubIssueMigrations(t, ctx, pool, githubIssueUpMigrations)
		if got := sandboxRelationCount(t, ctx, pool, schema, githubIssueTables); got != len(githubIssueTables) {
			t.Fatalf("round %d up: %d of %d tables exist", round, got, len(githubIssueTables))
		}
		var valid int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = ANY($2::text[]) AND i.indisvalid
		`, schema, githubIssueIndexes).Scan(&valid); err != nil {
			t.Fatalf("count valid indexes: %v", err)
		}
		if valid != len(githubIssueIndexes) {
			t.Fatalf("round %d up: %d of %d indexes valid", round, valid, len(githubIssueIndexes))
		}

		// Up is re-runnable: IF NOT EXISTS keeps a retried migration harmless.
		applyGithubIssueMigrations(t, ctx, pool, githubIssueUpMigrations)

		applyGithubIssueMigrations(t, ctx, pool, githubIssueDownMigrations)
		all := append(append([]string{}, githubIssueTables...), githubIssueIndexes...)
		if got := sandboxRelationCount(t, ctx, pool, schema, all); got != 0 {
			t.Fatalf("round %d down: %d relations left behind", round, got)
		}
	}
}

func testUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// TestGithubIssueWorkspaceTeardown covers the one query left after the
// feature was withdrawn (M-70): workspace teardown clears every fork table
// for that workspace and leaves other workspaces' rows alone.
func TestGithubIssueWorkspaceTeardown(t *testing.T) {
	const schema = "github_issue_teardown_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, githubIssueUpMigrations)
	q := db.New(pool)

	ws1 := testUUID(t, "00000000-0000-0000-0000-00000000a001")
	ws2 := testUUID(t, "00000000-0000-0000-0000-00000000a002")
	for _, ws := range []pgtype.UUID{ws1, ws2} {
		for _, stmt := range []string{
			`INSERT INTO issue_external (issue_id, workspace_id, provider, external_repo_id, external_repo, external_number, external_node_id, state, external_updated_at)
			 VALUES (gen_random_uuid(), $1, 'github', 42, 'octo/repo', 7, 'I_node', 'open', now())`,
			`INSERT INTO github_repo_sync (workspace_id, repo_id, owner, name) VALUES ($1, 42, 'octo', 'repo')`,
			`INSERT INTO github_write_intent (workspace_id, op, repo_id, external_repo, payload_digest, actor_type, actor_id)
			 VALUES ($1, 'create_issue', 42, 'octo/repo', 'd', 'member', gen_random_uuid())`,
			`INSERT INTO project_github_settings (project_id, workspace_id) VALUES (gen_random_uuid(), $1)`,
			`INSERT INTO comment_external (comment_id, workspace_id, external_id, login, external_updated_at)
			 VALUES (gen_random_uuid(), $1, 1, 'octocat', now())`,
		} {
			if _, err := pool.Exec(ctx, stmt, ws); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	if err := q.DeleteWorkspaceGithubIssueData(ctx, ws1); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, table := range githubIssueTables {
		var ws1Rows, ws2Rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE workspace_id = $1), count(*) FILTER (WHERE workspace_id = $2) FROM `+table,
			ws1, ws2,
		).Scan(&ws1Rows, &ws2Rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if ws1Rows != 0 {
			t.Errorf("%s: %d rows left for the deleted workspace", table, ws1Rows)
		}
		if ws2Rows != 1 {
			t.Errorf("%s: %d rows for the other workspace, want 1", table, ws2Rows)
		}
	}
}
