// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: connections keyed by entry ref (M-269).
// Modified for the mouse9527/multica fork: the pre-registered client table, 9027 (M-295).

package migrations

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The tables the fork's MCP OAuth migrations leave once all of them ran:
// 9026 drops mcp_oauth_server (and 9025 its index).
var mcpOAuthTables = []string{"mcp_oauth_connection", "mcp_oauth_state"}

var mcpOAuthRetired = []string{"mcp_oauth_server", "idx_mcp_oauth_server_agent_name"}

var mcpOAuthUp = []string{
	"9023_mcp_oauth_tables.up.sql",
	"9024_mcp_oauth_server_agent_name_idx.up.sql",
	"9025_drop_mcp_oauth_server_agent_name_idx.up.sql",
	"9026_mcp_oauth_server_ref.up.sql",
}

var mcpOAuthDown = []string{
	"9026_mcp_oauth_server_ref.down.sql",
	"9025_drop_mcp_oauth_server_agent_name_idx.down.sql",
	"9024_mcp_oauth_server_agent_name_idx.down.sql",
	"9023_mcp_oauth_tables.down.sql",
}

// TestMCPOAuthTablesMigrationRoundTrip: the ref-keyed tables and no
// mcp_oauth_server on up, re-runnable up, nothing left on down, and up again
// after a rollback. Rolling 9026 back alone restores the 9023/9024 shape.
func TestMCPOAuthTablesMigrationRoundTrip(t *testing.T) {
	const schema = "mcp_oauth_tables_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)

	column := func(table, name string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`, schema, table, name).Scan(&n); err != nil {
			t.Fatalf("look up column: %v", err)
		}
		return n == 1
	}

	for round := 1; round <= 2; round++ {
		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)
		if got := sandboxRelationCount(t, ctx, pool, schema, mcpOAuthTables); got != len(mcpOAuthTables) {
			t.Fatalf("round %d up: %d of %d tables exist", round, got, len(mcpOAuthTables))
		}
		if got := sandboxRelationCount(t, ctx, pool, schema, mcpOAuthRetired); got != 0 {
			t.Fatalf("round %d up: %d retired relations remain", round, got)
		}
		for _, table := range mcpOAuthTables {
			if !column(table, "server_ref") || !column(table, "server_url") || column(table, "server_id") {
				t.Fatalf("round %d up: %s is not keyed by server_ref", round, table)
			}
		}
		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)

		// 9026 and 9025 down give back exactly what 9024 left.
		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthDown[:2])
		if got := sandboxRelationCount(t, ctx, pool, schema, append(append([]string{}, mcpOAuthTables...), mcpOAuthRetired...)); got != 4 {
			t.Fatalf("round %d after rolling back 9026 and 9025: %d of 4 relations", round, got)
		}
		if !column("mcp_oauth_connection", "server_id") || column("mcp_oauth_connection", "server_ref") {
			t.Fatalf("round %d: rolling back 9026 did not restore the server_id key", round)
		}
		var valid int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = 'idx_mcp_oauth_server_agent_name' AND i.indisvalid AND i.indisunique
		`, schema).Scan(&valid); err != nil {
			t.Fatalf("count valid indexes: %v", err)
		}
		if valid != 1 {
			t.Fatalf("round %d: the 9024 index was not rebuilt valid", round)
		}

		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthDown[2:])
		all := append(append([]string{}, mcpOAuthTables...), mcpOAuthRetired...)
		if got := sandboxRelationCount(t, ctx, pool, schema, all); got != 0 {
			t.Fatalf("round %d down: %d relations left behind", round, got)
		}
	}
}

// TestMCPOAuthRefMigrationRefusesExistingGrants: 9026 replaces the old tables
// rather than converting them, so it stops while they hold a server or a
// connection instead of dropping a grant unnoticed.
func TestMCPOAuthRefMigrationRefusesExistingGrants(t *testing.T) {
	const schema = "mcp_oauth_ref_refuses_rows_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp[:3])
	if _, err := pool.Exec(ctx, `
		INSERT INTO mcp_oauth_server (workspace_id, agent_id, name, url, created_by)
		VALUES (gen_random_uuid(), gen_random_uuid(), 'gateway', 'https://mcp.example.com/mcp', gen_random_uuid())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readMigrationFile(t, mcpOAuthUp[3])); err == nil || !strings.Contains(err.Error(), "delete them") {
		t.Fatalf("9026 with an mcp_oauth_server row: err = %v, want a refusal", err)
	}
	if got := sandboxRelationCount(t, ctx, pool, schema, []string{"mcp_oauth_server"}); got != 1 {
		t.Fatal("the refused migration dropped mcp_oauth_server")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM mcp_oauth_server`); err != nil {
		t.Fatal(err)
	}
	applyMigrationFile(t, ctx, pool, mcpOAuthUp[3])
}

// TestMCPOAuthClientMigrationRoundTrip: 9027 only adds mcp_oauth_client, is
// re-runnable, and its down drops exactly that table.
func TestMCPOAuthClientMigrationRoundTrip(t *testing.T) {
	const schema = "mcp_oauth_client_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)
	client := []string{"mcp_oauth_client"}
	for round := 1; round <= 2; round++ {
		applyMigrationFile(t, ctx, pool, "9027_mcp_oauth_client.up.sql")
		applyMigrationFile(t, ctx, pool, "9027_mcp_oauth_client.up.sql")
		if got := sandboxRelationCount(t, ctx, pool, schema, client); got != 1 {
			t.Fatalf("round %d up: mcp_oauth_client missing", round)
		}
		if got := sandboxRelationCount(t, ctx, pool, schema, mcpOAuthTables); got != len(mcpOAuthTables) {
			t.Fatalf("round %d up: %d of %d earlier tables", round, got, len(mcpOAuthTables))
		}

		const ref = "ws:00000000-0000-0000-0000-0000000000a1"
		insert := `INSERT INTO mcp_oauth_client (server_ref, workspace_id, client_secret_ciphertext, updated_by)
			VALUES ($1, gen_random_uuid(), $2, gen_random_uuid())`
		if _, err := pool.Exec(ctx, insert, ref, []byte{}); err == nil {
			t.Fatal("an empty ciphertext was accepted")
		}
		if _, err := pool.Exec(ctx, insert, "gateway", []byte{1}); err == nil {
			t.Fatal("a malformed ref was accepted")
		}
		if _, err := pool.Exec(ctx, insert, ref, []byte{1}); err != nil {
			t.Fatalf("valid row: %v", err)
		}
		if _, err := pool.Exec(ctx, insert, ref, []byte{2}); err == nil {
			t.Fatal("a second secret for the same entry was accepted")
		}

		applyMigrationFile(t, ctx, pool, "9027_mcp_oauth_client.down.sql")
		if got := sandboxRelationCount(t, ctx, pool, schema, client); got != 0 {
			t.Fatalf("round %d down: mcp_oauth_client left behind", round)
		}
		if got := sandboxRelationCount(t, ctx, pool, schema, mcpOAuthTables); got != len(mcpOAuthTables) {
			t.Fatalf("round %d down: 9027 down dropped an earlier table", round)
		}
	}
}

// TestMCPOAuthTableConstraints pins the table-level guarantees the store
// relies on: a connected row has an access token, one connection per (entry,
// user), refs and URLs have their shape, and a state hash is exactly 32 bytes.
func TestMCPOAuthTableConstraints(t *testing.T) {
	const schema = "mcp_oauth_constraints_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)

	const wsRef = "ws:00000000-0000-0000-0000-0000000000a1"
	insertConnection := `
		INSERT INTO mcp_oauth_connection (server_ref, user_id, workspace_id, server_url, status, resource, token_endpoint, client_id, access_token_ciphertext)
		VALUES ($1, '00000000-0000-0000-0000-0000000000b1', gen_random_uuid(), $2, $3,
			'https://mcp.example.com/mcp', 'https://auth.example.com/token', 'client', $4)`
	if _, err := pool.Exec(ctx, insertConnection, wsRef, "https://mcp.example.com/mcp", "connected", nil); err == nil {
		t.Fatal("a connected row without an access token was accepted")
	}
	if _, err := pool.Exec(ctx, insertConnection, wsRef, "https://mcp.example.com/mcp", "connected", []byte{1}); err != nil {
		t.Fatalf("connected row: %v", err)
	}
	if _, err := pool.Exec(ctx, insertConnection, wsRef, "https://mcp.example.com/mcp", "connected", []byte{2}); err == nil {
		t.Fatal("a second connection for the same entry and user was accepted")
	}
	if _, err := pool.Exec(ctx, insertConnection, "ws:00000000-0000-0000-0000-0000000000a2", "https://mcp.example.com/mcp", "revoked", []byte{1}); err == nil {
		t.Fatal("an unknown status was accepted")
	}
	if _, err := pool.Exec(ctx, insertConnection, "agent:00000000-0000-0000-0000-0000000000c1:gateway", "https://mcp.example.com/mcp", "connected", []byte{1}); err != nil {
		t.Fatalf("agent ref: %v", err)
	}
	for _, ref := range []string{"gateway", "ws:not-a-uuid", "agent:00000000-0000-0000-0000-0000000000c1", "agent:00000000-0000-0000-0000-0000000000c1:bad name"} {
		if _, err := pool.Exec(ctx, insertConnection, ref, "https://mcp.example.com/mcp", "connected", []byte{1}); err == nil {
			t.Errorf("ref %q was accepted", ref)
		}
	}
	if _, err := pool.Exec(ctx, insertConnection, "ws:00000000-0000-0000-0000-0000000000a3", "http://mcp.example.com/mcp", "connected", []byte{1}); err == nil {
		t.Fatal("a non-HTTPS server URL was accepted")
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO mcp_oauth_state (state_hash, workspace_id, server_ref, server_url, user_id, resource, token_endpoint, client_id,
			redirect_uri, verifier_ciphertext, expires_at)
		VALUES ($1, gen_random_uuid(), $2, 'https://mcp.example.com/mcp', gen_random_uuid(), 'https://mcp.example.com/mcp',
			'https://auth.example.com/token', 'client', 'https://multica.example.com/cb', '\x01', now() + interval '10 minutes')`,
		[]byte("short"), wsRef); err == nil {
		t.Fatal("a state hash that is not 32 bytes was accepted")
	}
}

// TestMCPOAuthTablesStayInvisibleToSqlc: for the reason given on
// TestRouterTablesStayInvisibleToSqlc, table DDL runs inside DO blocks.
func TestMCPOAuthTablesStayInvisibleToSqlc(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(realMigrationsDir(t), "*_mcp_oauth_*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no MCP OAuth migrations found: %v", err)
	}
	doBlock := regexp.MustCompile(`(?s)DO \$[a-z_]+\$.*?END\s*\$[a-z_]+\$;`)
	created := 0
	for _, file := range files {
		src := readMigrationFile(t, filepath.Base(file))
		for _, block := range doBlock.FindAllString(src, -1) {
			created += strings.Count(block, "CREATE TABLE IF NOT EXISTS ")
		}
		outside := strings.ToUpper(stripSQLComments(doBlock.ReplaceAllString(src, "")))
		for _, ddl := range []string{"CREATE TABLE", "ALTER TABLE", "DROP TABLE"} {
			if strings.Contains(outside, ddl) {
				t.Errorf("%s has %s outside a DO block; sqlc would see it", filepath.Base(file), ddl)
			}
		}
	}
	// 9023 creates three tables; 9026 recreates two of them keyed by ref;
	// 9027 adds mcp_oauth_client.
	if created != 6 {
		t.Fatalf("DO blocks create %d MCP OAuth tables, want 6", created)
	}
}
