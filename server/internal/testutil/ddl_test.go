// Added by the mouse9527/multica fork.

package testutil

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestExecDDLLetsReadersPass replays the hang of two test packages: one holds
// a row lock in an open transaction and then reads the same table from
// another connection, while the other drops a trigger on that table. The read
// must return while the lock is still held, and the drop must complete once
// it is released.
func TestExecDDLLetsReadersPass(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The probe objects live in their own schema: tests in other packages scan
	// every table in public (the workspace deletion manifest check) and would
	// see a probe table created there while this test runs.
	schema := "ddl_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	table := pgx.Identifier{schema, "probe"}.Sanitize()
	fn := pgx.Identifier{schema, "probe_fn"}.Sanitize()
	trigger := pgx.Identifier{"probe_trg"}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schemaIdent+` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id int PRIMARY KEY);
		INSERT INTO `+table+` VALUES (1);
		CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$;
		CREATE TRIGGER `+trigger+` BEFORE UPDATE ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM `+table+` WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	dropped := make(chan error, 1)
	go func() { dropped <- ExecDDL(ctx, pool, `DROP TRIGGER `+trigger+` ON `+table) }()
	// Let the drop reach the lock queue before the read arrives.
	time.Sleep(300 * time.Millisecond)

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(readCtx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("read queued behind the drop while the row lock was held: %v", err)
	}

	select {
	case err := <-dropped:
		t.Fatalf("drop finished while the row lock was still held: %v", err)
	default:
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dropped:
		if err != nil {
			t.Fatalf("drop after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drop did not complete after the lock was released")
	}
}
