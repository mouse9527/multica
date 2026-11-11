// Added by the mouse9527/multica fork: DDL from tests that cannot wedge the
// test packages running beside them.

package testutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ddlLockTimeout = "200ms"
	ddlRetryWait   = 100 * time.Millisecond
	ddlDeadline    = 2 * time.Minute
)

// ExecDDL runs one DDL statement on a table other test packages use, such as
// the DROP TRIGGER that removes a fault-injection trigger.
//
// `go test ./...` runs package binaries in parallel against one database. A
// DROP TRIGGER needs ACCESS EXCLUSIVE on its table, so it waits behind any
// open transaction that touched the table, and every later read of the table
// queues behind the DROP. When the transaction it waits on belongs to a test
// that is itself waiting on such a read, nothing ever moves: the handler and
// service packages both hung until the 10-minute test timeout that way. Here
// the statement waits at most ddlLockTimeout, leaves the queue so the reads
// behind it proceed, and is retried until it gets the lock.
func ExecDDL(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	deadline := time.Now().Add(ddlDeadline)
	for {
		err := execDDLOnce(ctx, pool, sql)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ddl did not get its lock within %s: %w", ddlDeadline, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ddlRetryWait):
		}
	}
}

func execDDLOnce(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+ddlLockTimeout+"'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
