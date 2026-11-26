// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package crossread

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to the migrated shared test database. The tables have no
// foreign keys, so every test works with fresh random ids and cleans up only
// its own rows.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newIDs(t *testing.T, pool *pgxpool.Pool) (target, source, agent, other string) {
	t.Helper()
	target, source = uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM cross_workspace_read_audit WHERE target_workspace_id = $1`, target)
		pool.Exec(ctx, `DELETE FROM cross_workspace_read_grant WHERE target_workspace_id = $1`, target)
	})
	return target, source, uuid.NewString(), uuid.NewString()
}

func TestActiveGrant(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	target, source, agent, other := newIDs(t, pool)
	by := uuid.NewString()

	if _, found, err := store.ActiveGrant(ctx, target, source, agent); err != nil || found {
		t.Fatalf("no grant yet: found=%v err=%v", found, err)
	}

	specific, err := store.CreateGrant(ctx, CreateGrantParams{
		TargetWorkspaceID: target, SourceWorkspaceID: source, AgentID: agent, CreatedBy: by, Note: "one agent",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if specific.AgentID != agent || specific.CreatedBy != by || specific.ExpiresAt != nil || specific.RevokedAt != nil {
		t.Fatalf("created %+v", specific)
	}
	if g, found, err := store.ActiveGrant(ctx, target, source, agent); err != nil || !found || g.ID != specific.ID {
		t.Fatalf("agent grant: %+v found=%v err=%v", g, found, err)
	}
	// Another agent of the source, the reverse direction and another source
	// get nothing from it.
	for _, c := range [][3]string{{target, source, other}, {source, target, agent}, {target, other, agent}} {
		if _, found, err := store.ActiveGrant(ctx, c[0], c[1], c[2]); err != nil || found {
			t.Fatalf("%v: found=%v err=%v", c, found, err)
		}
	}

	// A grant for every agent covers the other agent; the agent's own grant
	// still wins for the agent.
	all, err := store.CreateGrant(ctx, CreateGrantParams{TargetWorkspaceID: target, SourceWorkspaceID: source, CreatedBy: by})
	if err != nil {
		t.Fatalf("create all-agents grant: %v", err)
	}
	if g, found, _ := store.ActiveGrant(ctx, target, source, other); !found || g.ID != all.ID || g.AgentID != "" {
		t.Fatalf("other agent: %+v found=%v", g, found)
	}
	if g, _, _ := store.ActiveGrant(ctx, target, source, agent); g.ID != specific.ID {
		t.Fatalf("agent got %s, want its own grant %s", g.ID, specific.ID)
	}

	// Revoking is immediate and keeps the first revocation.
	revoked, found, err := store.RevokeGrant(ctx, target, all.ID, by)
	if err != nil || !found || revoked.RevokedAt == nil || revoked.RevokedBy != by {
		t.Fatalf("revoke: %+v found=%v err=%v", revoked, found, err)
	}
	if _, found, _ := store.ActiveGrant(ctx, target, source, other); found {
		t.Fatal("a revoked grant still matches")
	}
	again, found, err := store.RevokeGrant(ctx, target, all.ID, uuid.NewString())
	if err != nil || !found || again.RevokedBy != by || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("second revoke changed the grant: %+v", again)
	}
	if _, found, err := store.RevokeGrant(ctx, source, specific.ID, by); err != nil || found {
		t.Fatalf("revoked another workspace's grant: found=%v err=%v", found, err)
	}

	// Expiry is checked against the database clock on every lookup.
	soon := time.Now().Add(time.Hour)
	if _, _, err := store.RevokeGrant(ctx, target, specific.ID, by); err != nil {
		t.Fatal(err)
	}
	expiring, err := store.CreateGrant(ctx, CreateGrantParams{
		TargetWorkspaceID: target, SourceWorkspaceID: source, AgentID: agent, CreatedBy: by, ExpiresAt: &soon,
	})
	if err != nil {
		t.Fatalf("create expiring grant: %v", err)
	}
	if _, found, _ := store.ActiveGrant(ctx, target, source, agent); !found {
		t.Fatal("an unexpired grant does not match")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE cross_workspace_read_grant
		SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second'
		WHERE id = $1`, expiring.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ActiveGrant(ctx, target, source, agent); found {
		t.Fatal("an expired grant still matches")
	}

	grants, err := store.ListGrants(ctx, target)
	if err != nil || len(grants) != 3 {
		t.Fatalf("list: %d grants, err=%v", len(grants), err)
	}
}

func TestAuditRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	target, source, agent, task := newIDs(t, pool)
	grant := uuid.NewString()

	entries := []AuditEntry{
		{GrantID: grant, TargetWorkspaceID: target, SourceWorkspaceID: source, AgentID: agent, TaskID: task,
			Method: "GET", Route: "GET /api/issues/*", Allowed: true},
		{GrantID: grant, TargetWorkspaceID: target, SourceWorkspaceID: source, AgentID: agent, TaskID: task,
			Method: "POST", Route: "POST /api/issues/x/comments", Allowed: false},
	}
	for _, e := range entries {
		if err := store.RecordAudit(ctx, e); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	rows, err := store.ListAudit(ctx, target, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %d rows err=%v", len(rows), err)
	}
	got := map[string]AuditEntry{}
	for _, r := range rows {
		got[r.Method] = r.AuditEntry
	}
	for _, e := range entries {
		if got[e.Method] != e {
			t.Fatalf("stored %+v, want %+v", got[e.Method], e)
		}
	}
	// A refused request's route is its path, cut to the column's limit.
	long := AuditEntry{GrantID: grant, TargetWorkspaceID: target, SourceWorkspaceID: source, AgentID: agent, TaskID: task,
		Method: "PUT", Route: "PUT /" + strings.Repeat("a", 600)}
	if err := store.RecordAudit(ctx, long); err != nil {
		t.Fatalf("record a long route: %v", err)
	}
	if rows, _ := store.ListAudit(ctx, target, 10); len(rows) != 3 || rows[0].Method != "PUT" || len(rows[0].Route) != maxRouteLen {
		t.Fatalf("long route: rows %+v", rows)
	}
	if one, _ := store.ListAudit(ctx, target, 1); len(one) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(one))
	}
}
