package githubissues

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestReminderThreshold(t *testing.T) {
	for daysLeft := -2; daysLeft <= 30; daysLeft++ {
		var want int
		switch {
		case daysLeft < 1 || daysLeft > 14:
			want = 0
		case daysLeft == 1:
			want = 1
		case daysLeft <= 7:
			want = 7
		default:
			want = 14
		}
		threshold, due := reminderThreshold(daysLeft)
		if due != (want != 0) || threshold != want {
			t.Errorf("daysLeft %d: got (%d, %v), want threshold %d", daysLeft, threshold, due, want)
		}
	}
}

func TestDaysUntilCountsUTCDates(t *testing.T) {
	expiry := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		now  time.Time
		want int
	}{
		{time.Date(2027, 1, 18, 0, 0, 0, 0, time.UTC), 14},
		{time.Date(2027, 1, 18, 23, 59, 0, 0, time.UTC), 14},
		{time.Date(2027, 1, 31, 12, 0, 0, 0, time.UTC), 1},
		{time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), 0},
		// 2027-01-25 20:00 in UTC-8 is already 2027-01-26 in UTC.
		{time.Date(2027, 1, 25, 20, 0, 0, 0, time.FixedZone("UTC-8", -8*3600)), 6},
	} {
		if got := daysUntil(tc.now, expiry); got != tc.want {
			t.Errorf("daysUntil(%s) = %d, want %d", tc.now, got, tc.want)
		}
	}
}

func TestExpiryReminderJobSpec(t *testing.T) {
	svc := newTestService(t, "http://127.0.0.1:0", "2027-02-01")
	job := svc.ExpiryReminderJob(nil, nil)
	if job.Name != "github_token_expiry" || job.Cadence != 24*time.Hour {
		t.Fatalf("job = %s every %s", job.Name, job.Cadence)
	}
	if job.Handler == nil || job.Scopes == nil {
		t.Fatal("job needs a handler and scopes")
	}
}

type reminderFixture struct {
	pool      *testutil.Fixture
	queries   *db.Queries
	workspace string
	owner     string
	admin     string
	member    string
	// otherOwner owns a workspace without GitHub repositories.
	otherOwner string
}

// newReminderFixture uses an isolated database: the recipients query scans
// every workspace, and on the shared test database it would also deliver
// reminders into other packages' fixtures.
func newReminderFixture(t *testing.T) reminderFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	f := testutil.New(pool, "", "")
	suffix := uuid.NewString()[:8]

	owner := f.User(t, "Owner", "owner-"+suffix+"@example.invalid")
	admin := f.User(t, "Admin", "admin-"+suffix+"@example.invalid")
	member := f.User(t, "Member", "member-"+suffix+"@example.invalid")
	ws := f.Workspace(t, "With repo", "with-repo-"+suffix)
	f.Member(t, ws, owner, "owner")
	f.Member(t, ws, admin, "admin")
	f.Member(t, ws, member, "member")
	f.WorkspaceID = ws
	project := f.Project(t, "Synced")
	f.Insert(t, "project_resource", testutil.Cols{
		"project_id":    project,
		"workspace_id":  ws,
		"resource_type": "github_repo",
		"resource_ref":  `{"url":"https://github.com/example/repo"}`,
	})

	otherOwner := f.User(t, "Other", "other-"+suffix+"@example.invalid")
	other := f.Workspace(t, "No repo", "no-repo-"+suffix)
	f.Member(t, other, otherOwner, "owner")

	return reminderFixture{
		pool: f, queries: db.New(pool), workspace: ws,
		owner: owner, admin: admin, member: member, otherOwner: otherOwner,
	}
}

// remindersFor returns the days_left of each reminder recipient got, oldest
// first.
func (r reminderFixture) remindersFor(t *testing.T, recipient string) []string {
	t.Helper()
	rows, err := r.pool.Pool.Query(context.Background(), `
		SELECT details->>'days_left' FROM inbox_item
		WHERE recipient_type = 'member' AND recipient_id = $1 AND type = 'github_token_expiry'
		ORDER BY created_at, id`, recipient)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var days string
		if err := rows.Scan(&days); err != nil {
			t.Fatal(err)
		}
		out = append(out, days)
	}
	return out
}

func TestExpiryRemindersAt14And7And1Days(t *testing.T) {
	fx := newReminderFixture(t)
	svc := newTestService(t, "http://127.0.0.1:0", "2027-02-01")
	bus := events.New()
	var published []events.Event
	bus.Subscribe(protocol.EventInboxNew, func(e events.Event) { published = append(published, e) })

	ctx := context.Background()
	// Walk the clock day by day, running the job twice a day the way a retry
	// or a second replica could.
	for day := time.Date(2027, 1, 1, 6, 0, 0, 0, time.UTC); day.Before(time.Date(2027, 2, 4, 0, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		for _, at := range []time.Time{day, day.Add(12 * time.Hour)} {
			if _, err := svc.sendExpiryReminders(ctx, fx.queries, bus, at); err != nil {
				t.Fatalf("%s: %v", at, err)
			}
		}
	}

	for _, who := range []string{fx.owner, fx.admin} {
		if got := strings.Join(fx.remindersFor(t, who), ","); got != "14,7,1" {
			t.Errorf("manager reminders at days_left %q, want 14,7,1", got)
		}
	}
	if got := fx.remindersFor(t, fx.member); len(got) != 0 {
		t.Errorf("plain member got reminders %v", got)
	}
	if got := fx.remindersFor(t, fx.otherOwner); len(got) != 0 {
		t.Errorf("workspace without GitHub repositories got reminders %v", got)
	}
	if len(published) != 6 {
		t.Errorf("published %d inbox:new events, want 6", len(published))
	}

	var title, body, severity, actor string
	fx.pool.QueryRow(t, `
		SELECT title, body, severity, actor_type FROM inbox_item
		WHERE recipient_id = $1 AND details->>'threshold_days' = '1'`, fx.owner).Scan(&title, &body, &severity, &actor)
	if title != "GitHub token expires tomorrow" || severity != "attention" || actor != "system" {
		t.Errorf("reminder = %q / %s / %s", title, severity, actor)
	}
	if !strings.Contains(body, "2027-02-01") || !strings.Contains(body, EnvTokenExpiresAt) {
		t.Errorf("body %q should name the date and the variable to update", body)
	}
	assertNoToken(t, "reminder body", body)
}

// A day the job did not run delays the reminder instead of dropping it.
func TestExpiryReminderCatchesUpAfterMissedDay(t *testing.T) {
	fx := newReminderFixture(t)
	svc := newTestService(t, "http://127.0.0.1:0", "2027-02-01")
	ctx := context.Background()
	for _, at := range []time.Time{
		time.Date(2027, 1, 17, 1, 0, 0, 0, time.UTC), // 15 days left: nothing due
		time.Date(2027, 1, 20, 1, 0, 0, 0, time.UTC), // 12 days left: the 14-day reminder
		time.Date(2027, 1, 21, 1, 0, 0, 0, time.UTC), // 11 days left: already sent
		time.Date(2027, 1, 28, 1, 0, 0, 0, time.UTC), // 4 days left: the 7-day reminder
	} {
		if _, err := svc.sendExpiryReminders(ctx, fx.queries, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(fx.remindersFor(t, fx.owner), ","); got != "12,4" {
		t.Errorf("reminders at days_left %q, want 12,4", got)
	}
}

func TestExpiryReminderNoneForNeverExpiringToken(t *testing.T) {
	svc := newTestService(t, "http://127.0.0.1:0", "never")
	// A nil Queries proves no database work happens.
	sent, err := svc.sendExpiryReminders(context.Background(), nil, nil, testNow)
	if err != nil || sent != 0 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
}
