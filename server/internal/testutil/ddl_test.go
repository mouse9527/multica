// Added by the mouse9527/multica fork.

package testutil

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	table, fn, trigger := "ddl_probe_"+suffix, "ddl_probe_fn_"+suffix, "ddl_probe_trg_"+suffix
	if _, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id int PRIMARY KEY);
		INSERT INTO `+table+` VALUES (1);
		CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$;
		CREATE TRIGGER `+trigger+` BEFORE UPDATE ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table+`; DROP FUNCTION IF EXISTS `+fn+`()`)
	})

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
