// Added by the mouse9527/multica fork: Evolution P2, cursors and runs (M-136).

package evolution

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the slice of a pgx pool or transaction the store uses. All SQL
// here is hand-written against tables sqlc does not see (migration 9019).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxStarter begins a transaction.
type TxStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Database is what the server hands the evolution package: a pgx pool.
type Database interface {
	Querier
	TxStarter
}

// Sources a run reads, and the member-notice cursor.
const (
	SourceComment      = "comment"
	SourceChat         = "chat"
	SourceMemberNotice = "member_notice"
)

// Run kinds and statuses.
const (
	RunScheduled = "scheduled"
	RunCatchUp   = "catch_up"

	RunOpen      = "open"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

var (
	ErrNoOpenRun = errors.New("no open Evolution analysis run for this task")
	ErrSourceOff = errors.New("this source is not being read in this workspace")
)

// Position is a keyset position over (created_at, id). An empty ID means
// every row at or before TS: it sorts after every row at TS.
type Position struct {
	TS time.Time
	ID string
}

// Before reports whether p sorts strictly before o.
func (p Position) Before(o Position) bool {
	if !p.TS.Equal(o.TS) {
		return p.TS.Before(o.TS)
	}
	if p.ID == "" {
		return false
	}
	return o.ID == "" || p.ID < o.ID
}

func (p Position) idArg() *string {
	if p.ID == "" {
		return nil
	}
	return &p.ID
}

// Token encodes p for a client: the after parameter of the next page.
func (p Position) Token() string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.TS.UTC().Format(time.RFC3339Nano) + "|" + p.ID))
}

// ParseToken decodes Token.
func ParseToken(s string) (Position, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Position{}, fmt.Errorf("after is not a page token")
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Position{}, fmt.Errorf("after is not a page token")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Position{}, fmt.Errorf("after is not a page token")
	}
	return Position{TS: t, ID: id}, nil
}

// GetCursor returns a source's cursor; ok is false when the source has none
// (it is not being read).
func GetCursor(ctx context.Context, q Querier, workspaceID, source string) (Position, bool, error) {
	var p Position
	var id *string
	err := q.QueryRow(ctx, `
		SELECT ts, item_id::text FROM evolution_cursor
		WHERE workspace_id = $1::uuid AND source = $2`, workspaceID, source).Scan(&p.TS, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, err
	}
	if id != nil {
		p.ID = *id
	}
	return p, true, nil
}

// SetCursor creates or moves a source's cursor to p.
func SetCursor(ctx context.Context, q Querier, workspaceID, source string, p Position) error {
	_, err := q.Exec(ctx, `
		INSERT INTO evolution_cursor (workspace_id, source, ts, item_id)
		VALUES ($1::uuid, $2, $3, $4::uuid)
		ON CONFLICT (workspace_id, source) DO UPDATE
		SET ts = EXCLUDED.ts, item_id = EXCLUDED.item_id, updated_at = now()`,
		workspaceID, source, p.TS, p.idArg())
	return err
}

// StartCursorIfMissing starts a source at p unless it already has a cursor.
func StartCursorIfMissing(ctx context.Context, q Querier, workspaceID, source string, p Position) error {
	_, err := q.Exec(ctx, `
		INSERT INTO evolution_cursor (workspace_id, source, ts, item_id)
		VALUES ($1::uuid, $2, $3, $4::uuid)
		ON CONFLICT (workspace_id, source) DO NOTHING`,
		workspaceID, source, p.TS, p.idArg())
	return err
}

// DeleteCursor stops reading a source. Reading it again starts from the
// moment it is turned back on; nothing in between is read.
func DeleteCursor(ctx context.Context, q Querier, workspaceID string, sources ...string) error {
	_, err := q.Exec(ctx, `
		DELETE FROM evolution_cursor WHERE workspace_id = $1::uuid AND source = ANY($2::text[])`,
		workspaceID, sources)
	return err
}

// advanceCursor moves an existing cursor forward to p. It never moves a
// cursor back (a source turned off and on again during a run starts later
// than what the run read) and never recreates a deleted one.
func advanceCursor(ctx context.Context, q Querier, workspaceID, source string, p Position) error {
	cur, ok, err := GetCursor(ctx, q, workspaceID, source)
	if err != nil || !ok || !cur.Before(p) {
		return err
	}
	_, err = q.Exec(ctx, `
		UPDATE evolution_cursor SET ts = $3, item_id = $4::uuid, updated_at = now()
		WHERE workspace_id = $1::uuid AND source = $2`,
		workspaceID, source, p.TS, p.idArg())
	return err
}

// Run is one analysis run.
type Run struct {
	ID            string
	WorkspaceID   string
	AgentID       string
	IssueID       string
	Kind          string
	Slot          *time.Time
	Status        string
	WindowUntil   time.Time
	Served        map[string]Position
	ServedItems   int
	FailureReason string
	CreatedAt     time.Time
	FinishedAt    *time.Time
}

const runColumns = `id::text, workspace_id::text, agent_id::text, COALESCE(issue_id::text, ''), kind, slot::timestamp,
	status, window_until, comment_served_ts, comment_served_id::text, chat_served_ts, chat_served_id::text,
	served_items, COALESCE(failure_reason, ''), created_at, finished_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var commentTS, chatTS *time.Time
	var commentID, chatID *string
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.AgentID, &r.IssueID, &r.Kind, &r.Slot,
		&r.Status, &r.WindowUntil, &commentTS, &commentID, &chatTS, &chatID,
		&r.ServedItems, &r.FailureReason, &r.CreatedAt, &r.FinishedAt)
	if err != nil {
		return Run{}, err
	}
	r.Served = map[string]Position{}
	for source, ts := range map[string]*time.Time{SourceComment: commentTS, SourceChat: chatTS} {
		if ts == nil {
			continue
		}
		p := Position{TS: *ts}
		id := commentID
		if source == SourceChat {
			id = chatID
		}
		if id != nil {
			p.ID = *id
		}
		r.Served[source] = p
	}
	return r, nil
}

// OpenRun returns the workspace's open run, if any.
func OpenRun(ctx context.Context, q Querier, workspaceID string) (Run, bool, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM evolution_run
		WHERE workspace_id = $1::uuid AND status = 'open' ORDER BY created_at DESC LIMIT 1`, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// openRunForIssueForUpdate locks the open run whose analysis issue is
// issueID, so concurrent page reads of one run serialize.
func openRunForIssueForUpdate(ctx context.Context, q Querier, workspaceID, issueID string) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM evolution_run
		WHERE workspace_id = $1::uuid AND issue_id = $2::uuid AND status = 'open'
		FOR UPDATE`, workspaceID, issueID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoOpenRun
	}
	return r, err
}

// LatestRun returns the workspace's most recent run.
func LatestRun(ctx context.Context, q Querier, workspaceID string) (Run, bool, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM evolution_run
		WHERE workspace_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// RecentRuns returns the workspace's latest runs, newest first.
func RecentRuns(ctx context.Context, q Querier, workspaceID string, limit int) ([]Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM evolution_run
		WHERE workspace_id = $1::uuid ORDER BY created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lastScheduledSlot is the slot date of the workspace's latest scheduled
// run, whatever its outcome.
func lastScheduledSlot(ctx context.Context, q Querier, workspaceID string) (*time.Time, error) {
	var slot *time.Time
	err := q.QueryRow(ctx, `
		SELECT max(slot)::timestamp FROM evolution_run
		WHERE workspace_id = $1::uuid AND kind = 'scheduled'`, workspaceID).Scan(&slot)
	return slot, err
}

// countRunsSince counts the runs created at or after since.
func countRunsSince(ctx context.Context, q Querier, workspaceID string, since time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM evolution_run WHERE workspace_id = $1::uuid AND created_at >= $2`,
		workspaceID, since).Scan(&n)
	return n, err
}

func insertRun(ctx context.Context, q Querier, workspaceID, agentID, kind string, slot *time.Time, until time.Time) (Run, error) {
	var slotArg any
	if slot != nil {
		slotArg = slot.Format(time.DateOnly)
	}
	return scanRun(q.QueryRow(ctx, `
		INSERT INTO evolution_run (workspace_id, agent_id, kind, slot, window_until)
		VALUES ($1::uuid, $2::uuid, $3, $4::date, $5)
		RETURNING `+runColumns, workspaceID, agentID, kind, slotArg, until))
}

func setRunIssue(ctx context.Context, q Querier, runID, issueID string) error {
	_, err := q.Exec(ctx, `UPDATE evolution_run SET issue_id = $2::uuid WHERE id = $1::uuid`, runID, issueID)
	return err
}

func finishRun(ctx context.Context, q Querier, runID, status, reason string) error {
	_, err := q.Exec(ctx, `
		UPDATE evolution_run SET status = $2, failure_reason = NULLIF($3, ''), finished_at = now()
		WHERE id = $1::uuid AND status = 'open'`, runID, status, reason)
	return err
}

func recordServed(ctx context.Context, q Querier, runID, source string, p Position, newItems int) error {
	column := "comment"
	if source == SourceChat {
		column = "chat"
	}
	_, err := q.Exec(ctx, `
		UPDATE evolution_run SET `+column+`_served_ts = $2, `+column+`_served_id = $3::uuid,
		       served_items = served_items + $4
		WHERE id = $1::uuid`, runID, p.TS, p.idArg(), newItems)
	return err
}

// DeleteWorkspaceData removes every Evolution row of a workspace. It runs
// inside the workspace delete transaction; the tables have no foreign keys.
func DeleteWorkspaceData(ctx context.Context, q Querier, workspaceID any) error {
	for _, table := range []string{"evolution_cursor", "evolution_run", "evolution_proposal", "evolution_config_version"} {
		if _, err := q.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspaceID); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return nil
}
