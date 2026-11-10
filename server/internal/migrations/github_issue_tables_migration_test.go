// Modified for the mouse9527/multica fork: GitHub-native issues.

package migrations

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func testUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

func ts(v time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: v, Valid: true}
}

const (
	ghWS1 = "00000000-0000-0000-0000-00000000a001"
	ghWS2 = "00000000-0000-0000-0000-00000000a002"
)

// TestGithubIssueQueries exercises the invariants the sync and dual-write
// tasks rely on: one local row per GitHub object, stale snapshots never win,
// cursors never move back, settled intents are final, and teardown is scoped
// to one workspace.
func TestGithubIssueQueries(t *testing.T) {
	const schema = "github_issue_queries_test"
	ctx := context.Background()
	pool := newGithubIssueSandbox(t, schema)
	applyGithubIssueMigrations(t, ctx, pool, githubIssueUpMigrations)
	q := db.New(pool)

	ws1, ws2 := testUUID(t, ghWS1), testUUID(t, ghWS2)
	t0 := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

	t.Run("issue_external", func(t *testing.T) {
		issueA := testUUID(t, "00000000-0000-0000-0000-0000000000a1")
		params := db.UpsertIssueExternalParams{
			IssueID: issueA, WorkspaceID: ws1, Provider: "github",
			ExternalRepoID: 42, ExternalRepo: "octo/repo", ExternalNumber: 7,
			ExternalNodeID: "I_node7", State: "open", ExternalUpdatedAt: ts(t0),
		}
		if _, err := q.UpsertIssueExternal(ctx, params); err != nil {
			t.Fatalf("insert: %v", err)
		}

		newer := params
		newer.State = "closed"
		newer.StateReason = pgtype.Text{String: "completed", Valid: true}
		newer.ExternalUpdatedAt = ts(t0.Add(time.Minute))
		row, err := q.UpsertIssueExternal(ctx, newer)
		if err != nil {
			t.Fatalf("newer upsert: %v", err)
		}
		if row.State != "closed" || row.StateReason.String != "completed" {
			t.Fatalf("newer snapshot not applied: %+v", row)
		}

		stale := params
		stale.State = "open"
		stale.ExternalRepo = "octo/old-name"
		row, err = q.UpsertIssueExternal(ctx, stale)
		if err != nil {
			t.Fatalf("stale upsert: %v", err)
		}
		if row.State != "closed" || row.ExternalRepo != "octo/repo" || !row.ExternalUpdatedAt.Time.Equal(t0.Add(time.Minute)) {
			t.Fatalf("stale snapshot overwrote newer state: %+v", row)
		}

		// A second local issue cannot claim the same GitHub issue.
		dup := params
		dup.IssueID = testUUID(t, "00000000-0000-0000-0000-0000000000a2")
		if _, err := q.UpsertIssueExternal(ctx, dup); !isUniqueViolation(err) {
			t.Fatalf("duplicate GitHub issue: got %v, want unique violation", err)
		}
		// The same GitHub issue may be mirrored into another workspace.
		other := dup
		other.WorkspaceID = ws2
		if _, err := q.UpsertIssueExternal(ctx, other); err != nil {
			t.Fatalf("same GitHub issue in another workspace: %v", err)
		}
		// An issue id cannot be re-homed into another workspace by upsert.
		hijack := params
		hijack.WorkspaceID = ws2
		hijack.ExternalNumber = 8
		if _, err := q.UpsertIssueExternal(ctx, hijack); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("cross-workspace upsert: got %v, want no rows", err)
		}

		got, err := q.GetIssueExternalByNumber(ctx, db.GetIssueExternalByNumberParams{
			WorkspaceID: ws1, Provider: "github", ExternalRepoID: 42, ExternalNumber: 7,
		})
		if err != nil || got.IssueID != issueA {
			t.Fatalf("lookup by number: %+v, %v", got, err)
		}
		rows, err := q.ListIssueExternalsByIssueIDs(ctx, db.ListIssueExternalsByIssueIDsParams{
			WorkspaceID: ws1, IssueIds: []pgtype.UUID{issueA, dup.IssueID},
		})
		if err != nil || len(rows) != 1 || rows[0].IssueID != issueA {
			t.Fatalf("batch lookup must return only ws1's row: %+v, %v", rows, err)
		}
	})

	t.Run("github_repo_sync", func(t *testing.T) {
		if _, err := q.UpsertGithubRepoSync(ctx, db.UpsertGithubRepoSyncParams{
			WorkspaceID: ws1, RepoID: 42, Owner: "octo", Name: "repo",
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		row, err := q.AdvanceGithubRepoSyncCursors(ctx, db.AdvanceGithubRepoSyncCursorsParams{
			WorkspaceID: ws1, RepoID: 42, IssuesCursor: ts(t0.Add(time.Hour)),
		})
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if !row.IssuesCursor.Time.Equal(t0.Add(time.Hour)) || row.CommentsCursor.Valid || !row.LastPolledAt.Valid {
			t.Fatalf("first advance: %+v", row)
		}
		row, err = q.AdvanceGithubRepoSyncCursors(ctx, db.AdvanceGithubRepoSyncCursorsParams{
			WorkspaceID: ws1, RepoID: 42, IssuesCursor: ts(t0), CommentsCursor: ts(t0),
		})
		if err != nil {
			t.Fatalf("advance backwards: %v", err)
		}
		if !row.IssuesCursor.Time.Equal(t0.Add(time.Hour)) || !row.CommentsCursor.Time.Equal(t0) {
			t.Fatalf("cursor moved backwards or NULL cursor not set: %+v", row)
		}

		// A rename refreshes owner/name without resetting progress.
		row, err = q.UpsertGithubRepoSync(ctx, db.UpsertGithubRepoSyncParams{
			WorkspaceID: ws1, RepoID: 42, Owner: "octo", Name: "renamed",
		})
		if err != nil || row.Name != "renamed" || !row.IssuesCursor.Valid {
			t.Fatalf("rename lost cursors: %+v, %v", row, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE github_repo_sync SET status = 'bogus'`); err == nil {
			t.Fatal("unknown status accepted")
		}
	})

	t.Run("github_write_intent", func(t *testing.T) {
		intent, err := q.CreateGithubWriteIntent(ctx, db.CreateGithubWriteIntentParams{
			WorkspaceID: ws1, Op: "create_comment", RepoID: 42, ExternalRepo: "octo/repo",
			ExternalNumber: pgtype.Int4{Int32: 7, Valid: true},
			PayloadDigest:  "sha256:abc", ActorType: "agent",
			ActorID: testUUID(t, "00000000-0000-0000-0000-0000000000b1"),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if intent.State != "pending" || !intent.ID.Valid {
			t.Fatalf("new intent: %+v", intent)
		}

		row, err := q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
			ID: intent.ID, WorkspaceID: ws1, State: "github_ok",
			GithubID: pgtype.Int8{Int64: 9001, Valid: true},
		})
		if err != nil || row.GithubID.Int64 != 9001 {
			t.Fatalf("github_ok: %+v, %v", row, err)
		}
		open, err := q.ListOpenGithubWriteIntents(ctx, db.ListOpenGithubWriteIntentsParams{
			WorkspaceID: ws1, CreatedBefore: ts(time.Now().Add(time.Minute)), RowLimit: 10,
		})
		if err != nil || len(open) != 1 {
			t.Fatalf("open intents: %+v, %v", open, err)
		}

		row, err = q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
			ID: intent.ID, WorkspaceID: ws1, State: "applied",
			CommentID: testUUID(t, "00000000-0000-0000-0000-0000000000c1"),
		})
		if err != nil || row.State != "applied" || row.GithubID.Int64 != 9001 || !row.CommentID.Valid {
			t.Fatalf("applied must keep github_id and record comment: %+v, %v", row, err)
		}
		if _, err := q.UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
			ID: intent.ID, WorkspaceID: ws1, State: "unknown",
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("settled intent changed state: %v", err)
		}
		open, err = q.ListOpenGithubWriteIntents(ctx, db.ListOpenGithubWriteIntentsParams{
			WorkspaceID: ws1, CreatedBefore: ts(time.Now().Add(time.Minute)), RowLimit: 10,
		})
		if err != nil || len(open) != 0 {
			t.Fatalf("applied intent still listed as open: %+v, %v", open, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE github_write_intent SET op = 'bogus'`); err == nil {
			t.Fatal("unknown op accepted")
		}
	})

	t.Run("project_github_settings", func(t *testing.T) {
		project := testUUID(t, "00000000-0000-0000-0000-0000000000d1")
		row, err := q.UpsertProjectGithubSettings(ctx, db.UpsertProjectGithubSettingsParams{
			ProjectID: project, WorkspaceID: ws1, StatusLabelMap: []byte(`{"todo":"status:todo"}`),
		})
		if err != nil || row.DefaultResourceID.Valid {
			t.Fatalf("insert: %+v, %v", row, err)
		}
		resource := testUUID(t, "00000000-0000-0000-0000-0000000000d2")
		row, err = q.UpsertProjectGithubSettings(ctx, db.UpsertProjectGithubSettingsParams{
			ProjectID: project, WorkspaceID: ws1, DefaultResourceID: resource, StatusLabelMap: []byte(`{}`),
		})
		if err != nil || row.DefaultResourceID != resource {
			t.Fatalf("update: %+v, %v", row, err)
		}
		if _, err := q.UpsertProjectGithubSettings(ctx, db.UpsertProjectGithubSettingsParams{
			ProjectID: project, WorkspaceID: ws2, StatusLabelMap: []byte(`{}`),
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("cross-workspace upsert: got %v, want no rows", err)
		}
	})

	t.Run("comment_external", func(t *testing.T) {
		comment := testUUID(t, "00000000-0000-0000-0000-0000000000e1")
		params := db.UpsertCommentExternalParams{
			CommentID: comment, WorkspaceID: ws1, ExternalID: 5_000_000_000,
			Login: "someone", ExternalUpdatedAt: ts(t0.Add(time.Minute)),
		}
		if _, err := q.UpsertCommentExternal(ctx, params); err != nil {
			t.Fatalf("insert: %v", err)
		}
		stale := params
		stale.Login = "old-login"
		stale.ExternalUpdatedAt = ts(t0)
		row, err := q.UpsertCommentExternal(ctx, stale)
		if err != nil || row.Login != "someone" {
			t.Fatalf("stale snapshot overwrote newer author: %+v, %v", row, err)
		}
		dup := params
		dup.CommentID = testUUID(t, "00000000-0000-0000-0000-0000000000e2")
		if _, err := q.UpsertCommentExternal(ctx, dup); !isUniqueViolation(err) {
			t.Fatalf("duplicate GitHub comment: got %v, want unique violation", err)
		}
		got, err := q.GetCommentExternalByExternalID(ctx, db.GetCommentExternalByExternalIDParams{
			WorkspaceID: ws1, ExternalID: 5_000_000_000,
		})
		if err != nil || got.CommentID != comment {
			t.Fatalf("lookup by GitHub id: %+v, %v", got, err)
		}
	})

	t.Run("workspace teardown", func(t *testing.T) {
		if _, err := q.UpsertGithubRepoSync(ctx, db.UpsertGithubRepoSyncParams{
			WorkspaceID: ws2, RepoID: 42, Owner: "octo", Name: "repo",
		}); err != nil {
			t.Fatalf("register ws2 repo: %v", err)
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
			if (table == "issue_external" || table == "github_repo_sync") && ws2Rows == 0 {
				t.Errorf("%s: teardown removed another workspace's rows", table)
			}
		}
	})
}
