// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package migrations

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mcpOAuthTables = []string{"mcp_oauth_server", "mcp_oauth_connection", "mcp_oauth_state"}

var mcpOAuthIndexes = []string{"idx_mcp_oauth_server_agent_name"}

var mcpOAuthUp = []string{"9023_mcp_oauth_tables.up.sql", "9024_mcp_oauth_server_agent_name_idx.up.sql"}

var mcpOAuthDown = []string{"9024_mcp_oauth_server_agent_name_idx.down.sql", "9023_mcp_oauth_tables.down.sql"}

// TestMCPOAuthTablesMigrationRoundTrip: tables and a valid index on up,
// re-runnable up, nothing left on down, and up again after a rollback.
func TestMCPOAuthTablesMigrationRoundTrip(t *testing.T) {
	const schema = "mcp_oauth_tables_round_trip_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)

	for round := 1; round <= 2; round++ {
		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)
		if got := sandboxRelationCount(t, ctx, pool, schema, mcpOAuthTables); got != len(mcpOAuthTables) {
			t.Fatalf("round %d up: %d of %d tables exist", round, got, len(mcpOAuthTables))
		}
		var valid int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = ANY($2::text[]) AND i.indisvalid AND i.indisunique
		`, schema, mcpOAuthIndexes).Scan(&valid); err != nil {
			t.Fatalf("count valid indexes: %v", err)
		}
		if valid != len(mcpOAuthIndexes) {
			t.Fatalf("round %d up: %d of %d unique indexes valid", round, valid, len(mcpOAuthIndexes))
		}
		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)

		applyGithubIssueMigrations(t, ctx, pool, mcpOAuthDown)
		all := append(append([]string{}, mcpOAuthTables...), mcpOAuthIndexes...)
		if got := sandboxRelationCount(t, ctx, pool, schema, all); got != 0 {
			t.Fatalf("round %d down: %d relations left behind", round, got)
		}
	}
}

// TestMCPOAuthTableConstraints pins the table-level
// guarantees the store relies on: a connected row has an access token, one
// connection per (server, user), and a state hash is exactly 32 bytes.
func TestMCPOAuthTableConstraints(t *testing.T) {
	const schema = "mcp_oauth_constraints_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, mcpOAuthUp)

	insertConnection := `
		INSERT INTO mcp_oauth_connection (server_id, user_id, workspace_id, status, resource, token_endpoint, client_id, access_token_ciphertext)
		VALUES ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-0000000000b1', gen_random_uuid(), $1,
			'https://mcp.example.com/mcp', 'https://auth.example.com/token', 'client', $2)`
	if _, err := pool.Exec(ctx, insertConnection, "connected", nil); err == nil {
		t.Fatal("a connected row without an access token was accepted")
	}
	if _, err := pool.Exec(ctx, insertConnection, "connected", []byte{1}); err != nil {
		t.Fatalf("connected row: %v", err)
	}
	if _, err := pool.Exec(ctx, insertConnection, "connected", []byte{2}); err == nil {
		t.Fatal("a second connection for the same server and user was accepted")
	}
	if _, err := pool.Exec(ctx, insertConnection, "revoked", []byte{1}); err == nil {
		t.Fatal("an unknown status was accepted")
	}

	insertServer := `
		INSERT INTO mcp_oauth_server (workspace_id, agent_id, name, url, created_by)
		VALUES (gen_random_uuid(), '00000000-0000-0000-0000-0000000000c1', $1, $2, gen_random_uuid())`
	if _, err := pool.Exec(ctx, insertServer, "gateway", "http://mcp.example.com/mcp"); err == nil {
		t.Fatal("a non-HTTPS server URL was accepted")
	}
	if _, err := pool.Exec(ctx, insertServer, "bad name", "https://mcp.example.com/mcp"); err == nil {
		t.Fatal("an invalid server name was accepted")
	}
	if _, err := pool.Exec(ctx, insertServer, "gateway", "https://mcp.example.com/mcp"); err != nil {
		t.Fatalf("server: %v", err)
	}
	if _, err := pool.Exec(ctx, insertServer, "gateway", "https://other.example.com/mcp"); err == nil {
		t.Fatal("a second server with the same name on one agent was accepted")
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO mcp_oauth_state (state_hash, workspace_id, server_id, user_id, resource, token_endpoint, client_id,
			redirect_uri, verifier_ciphertext, expires_at)
		VALUES ($1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'https://mcp.example.com/mcp',
			'https://auth.example.com/token', 'client', 'https://multica.example.com/cb', '\x01', now() + interval '10 minutes')`,
		[]byte("short")); err == nil {
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
		for _, ddl := range []string{"CREATE TABLE", "ALTER TABLE"} {
			if strings.Contains(outside, ddl) {
				t.Errorf("%s has %s outside a DO block; sqlc would see it", filepath.Base(file), ddl)
			}
		}
	}
	if created != len(mcpOAuthTables) {
		t.Fatalf("DO blocks create %d MCP OAuth tables, want %d", created, len(mcpOAuthTables))
	}
}
