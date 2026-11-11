package githubissues

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testMapping(labels map[string]string) statusMapping {
	m := statusMapping{entry: issuestatus.Backlog, labels: map[string]string{}, keyOfLabel: map[string]string{},
		category: func(key string) string {
			switch key {
			case issuestatus.Done, "shipped":
				return issuestatus.CategoryDone
			case issuestatus.Cancelled, "wontfix":
				return issuestatus.CategoryClosed
			case issuestatus.Backlog, issuestatus.Todo:
				return issuestatus.CategoryUnstarted
			}
			return issuestatus.CategoryStarted
		}}
	for key, label := range labels {
		m.labels[key] = label
		m.keyOfLabel[strings.ToLower(label)] = key
	}
	return m
}

func labelsOf(names ...string) []githubLabel {
	out := make([]githubLabel, len(names))
	for i, name := range names {
		out[i] = githubLabel{Name: name}
	}
	return out
}

func TestStatusFromGitHub(t *testing.T) {
	m := testMapping(map[string]string{
		"in_progress": "status:in-progress", "in_review": "status:review", "blocked": "Status:Blocked",
		"shipped": "status:shipped",
	})
	cases := []struct {
		name          string
		state, reason string
		labels        []string
		current       string
		want          string
		ambiguous     bool
	}{
		{"closed completed", "closed", "completed", nil, "in_progress", "done", false},
		{"closed without reason", "closed", "", nil, "backlog", "done", false},
		{"closed keeps a done-category status", "closed", "completed", nil, "shipped", "shipped", false},
		{"closed not planned", "closed", "not_planned", nil, "in_progress", "cancelled", false},
		{"closed duplicate", "closed", "duplicate", nil, "todo", "cancelled", false},
		{"closed not planned keeps a cancelled-category status", "closed", "not_planned", nil, "wontfix", "wontfix", false},
		{"closed not planned leaves done", "closed", "not_planned", nil, "done", "cancelled", false},
		{"new closed issue", "closed", "completed", nil, "", "done", false},
		{"open without labels", "open", "", nil, "in_progress", "backlog", false},
		{"reopened", "open", "reopened", nil, "done", "backlog", false},
		{"open with a mapped label", "open", "", []string{"bug", "status:in-progress"}, "backlog", "in_progress", false},
		{"labels match without regard to case", "open", "", []string{"STATUS:REVIEW"}, "backlog", "in_review", false},
		{"unknown status label is ignored", "open", "", []string{"status:someday"}, "in_progress", "backlog", false},
		{"label for a done status is ignored", "open", "", []string{"status:shipped"}, "backlog", "backlog", false},
		{"the label added after the current status wins", "open", "", []string{"status:review", "status:in-progress"}, "in_review", "in_progress", false},
		{"the same, in the other label order", "open", "", []string{"status:in-progress", "status:review"}, "in_review", "in_progress", false},
		{"undecidable: last label wins", "open", "", []string{"status:review", "status:blocked", "status:in-progress"}, "in_review", "in_progress", true},
		{"undecidable from the entry status", "open", "", []string{"status:blocked", "status:review"}, "backlog", "in_review", true},
		{"repeated label counts once", "open", "", []string{"status:review", "Status:Review"}, "backlog", "in_review", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ambiguous := m.fromGitHub(tc.state, tc.reason, labelsOf(tc.labels...), tc.current)
			if got != tc.want || ambiguous != tc.ambiguous {
				t.Fatalf("fromGitHub = %q (ambiguous %v), want %q (ambiguous %v)", got, ambiguous, tc.want, tc.ambiguous)
			}
		})
	}
}

func TestStatusMappable(t *testing.T) {
	m := testMapping(map[string]string{"in_progress": "status:in-progress"})
	for key, want := range map[string]bool{
		"backlog": true, "done": true, "shipped": true, "cancelled": true, "wontfix": true,
		"in_progress": true, "in_review": false, "todo": false, "blocked": false,
	} {
		if got := m.mappable(key); got != want {
			t.Errorf("mappable(%q) = %v, want %v", key, got, want)
		}
	}
	if got := m.shown(); !slices.Equal(got, []string{"backlog", "in_progress"}) {
		t.Fatalf("shown = %v", got)
	}
}

// recordingStatusWriter stands in for the issue write path: it records each
// call and applies the status directly, or fails with err.
type recordingStatusWriter struct {
	mu     sync.Mutex
	calls  []statusWrite
	err    error
	apply  func(issue db.Issue, status string)
	synced []bool
}

type statusWrite struct {
	actor, issue, status string
}

func (w *recordingStatusWriter) WriteGitHubStatus(ctx context.Context, userID pgtype.UUID, issue db.Issue, status string) error {
	w.mu.Lock()
	w.calls = append(w.calls, statusWrite{util.UUIDToString(userID), util.UUIDToString(issue.ID), status})
	w.synced = append(w.synced, IsSyncedWrite(ctx))
	err := w.err
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if w.apply != nil {
		w.apply(issue, status)
	}
	return nil
}

func (w *recordingStatusWriter) take() []statusWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	calls := w.calls
	w.calls = nil
	return calls
}

func newStatusEnv(t *testing.T) (*pollEnv, *recordingStatusWriter) {
	t.Helper()
	env := newPollEnv(t)
	w := &recordingStatusWriter{}
	w.apply = func(issue db.Issue, status string) {
		env.f.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, util.UUIDToString(issue.ID), status)
	}
	env.svc.SetStatusWriter(w)
	return env, w
}

// TestPollSyncsStatusThroughTheWritePath: a GitHub issue closed outside
// Multica reaches the status write path as done (or cancelled for not
// planned), mapped status labels move an open issue, and a snapshot whose
// state already matches the Multica status writes nothing.
func TestPollSyncsStatusThroughTheWritePath(t *testing.T) {
	env, w := newStatusEnv(t)
	env.f.Exec(t, `INSERT INTO project_github_settings (project_id, workspace_id, status_label_map) VALUES ($1, $2, $3)`,
		env.project, env.ws, `{"in_progress":"status:in-progress","in_review":"status:review"}`)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash", "", "open", t0, nil)
	env.api.putIssue(2, "stranger", "Typo", "", "open", t0, map[string]any{"labels": []any{map[string]any{"name": "status:review"}}})
	env.poll(t)
	if got := env.issue(t, 1).status; got != "backlog" {
		t.Fatalf("issue 1 starts on %q, want backlog", got)
	}
	if got := env.issue(t, 2).status; got != "in_review" {
		t.Fatalf("issue 2 with status:review starts on %q, want in_review", got)
	}
	if calls := w.take(); len(calls) != 0 {
		t.Fatalf("creating issues wrote statuses: %+v", calls)
	}

	// Closed on GitHub as completed: done, written as the sync account.
	env.api.putIssue(1, "stranger", "Crash", "", "closed", t0.Add(time.Minute), map[string]any{"state_reason": "completed"})
	// Moved on GitHub by a label.
	env.api.putIssue(2, "stranger", "Typo", "", "open", t0.Add(time.Minute), map[string]any{"labels": []any{map[string]any{"name": "status:in-progress"}}})
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	calls := w.take()
	want := []statusWrite{{env.owner, env.issue(t, 1).id, "done"}, {env.owner, env.issue(t, 2).id, "in_progress"}}
	if !slices.Equal(calls, want) {
		t.Fatalf("status writes = %+v, want %+v", calls, want)
	}
	if slices.Contains(w.synced, false) {
		t.Fatal("a status write from GitHub was not marked as synced, so it could be written back")
	}

	// A title edit leaves state and labels as they are: no status write.
	env.api.putIssue(1, "stranger", "Crash on start", "", "closed", t0.Add(2*time.Minute), map[string]any{"state_reason": "completed"})
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t)
	if calls := w.take(); len(calls) != 0 {
		t.Fatalf("unchanged state wrote statuses: %+v", calls)
	}

	// Reopened, then closed as not planned.
	env.api.putIssue(1, "stranger", "Crash on start", "", "open", t0.Add(3*time.Minute), map[string]any{"state_reason": "reopened"})
	*env.now = testNow.Add(3 * pollInterval)
	env.poll(t)
	env.api.putIssue(1, "stranger", "Crash on start", "", "closed", t0.Add(4*time.Minute), map[string]any{"state_reason": "not_planned"})
	*env.now = testNow.Add(4 * pollInterval)
	env.poll(t)
	id := env.issue(t, 1).id
	if calls := w.take(); !slices.Equal(calls, []statusWrite{{env.owner, id, "backlog"}, {env.owner, id, "cancelled"}}) {
		t.Fatalf("reopen and close as not planned wrote %+v", calls)
	}
}

// A status the write path refuses (the workflow does not list it) is not
// retried, while any other failure leaves the issue to be read again.
func TestPollStatusWriteFailures(t *testing.T) {
	env, w := newStatusEnv(t)
	t0 := testNow.Add(-time.Hour)
	env.api.putIssue(1, "stranger", "Crash", "", "open", t0, nil)
	env.poll(t)

	env.api.putIssue(1, "stranger", "Crash", "", "closed", t0.Add(time.Minute), nil)
	w.err = &StatusWriteError{Code: 400, Message: "status not in workflow"}
	*env.now = testNow.Add(pollInterval)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("a refused status failed the repository: %+v", stats)
	}
	*env.now = testNow.Add(2 * pollInterval)
	env.poll(t) // re-read in the overlap window, refused again
	if calls := w.take(); len(calls) != 2 {
		t.Fatalf("refused writes = %+v", calls)
	}

	w.err = errors.New("database unavailable")
	*env.now = testNow.Add(3 * pollInterval)
	if stats := env.poll(t); stats.Failed != 1 {
		t.Fatalf("a failed status write must fail the repository so the issue is read again: %+v", stats)
	}
	w.err = nil
	w.take()
	*env.now = testNow.Add(4 * pollInterval)
	if stats := env.poll(t); stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if calls := w.take(); len(calls) != 1 || calls[0].status != "done" {
		t.Fatalf("retry wrote %+v", calls)
	}
	if got := env.issue(t, 1).status; got != "done" {
		t.Fatalf("status after retry = %q", got)
	}
}

// CheckGitHubStatus refuses statuses GitHub cannot show for a GitHub-backed
// issue and lets every status through for a local one.
func TestCheckGitHubStatus(t *testing.T) {
	env, _ := newStatusEnv(t)
	env.f.Exec(t, `INSERT INTO project_github_settings (project_id, workspace_id, status_label_map) VALUES ($1, $2, $3)`,
		env.project, env.ws, `{"in_progress":"status:in-progress","bogus":"in-review"}`)
	env.api.putIssue(1, "stranger", "Crash", "", "open", testNow.Add(-time.Hour), nil)
	env.poll(t)
	ctx := context.Background()
	q := db.New(env.svc.db)
	issue, err := q.GetIssue(ctx, util.MustParseUUID(env.issue(t, 1).id))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"backlog", "done", "cancelled", "in_progress"} {
		if err := env.svc.CheckGitHubStatus(ctx, issue, issue.ProjectID, status); err != nil {
			t.Errorf("%s refused: %v", status, err)
		}
	}
	err = env.svc.CheckGitHubStatus(ctx, issue, issue.ProjectID, "in_review")
	var unmapped *UnmappedStatusError
	if !errors.As(err, &unmapped) || !strings.Contains(err.Error(), "acme/widgets#1") ||
		!slices.Equal(unmapped.Mappable, []string{"backlog", "in_progress"}) {
		t.Fatalf("in_review: %v", err)
	}
	// Outside the project the map does not apply.
	if err := env.svc.CheckGitHubStatus(ctx, issue, pgtype.UUID{}, "in_progress"); err == nil {
		t.Fatal("in_progress passed outside the project that maps it")
	}

	local := env.f.Issue(t, "local issue", testutil.Cols{"project_id": env.project})
	localIssue, err := q.GetIssue(ctx, util.MustParseUUID(local))
	if err != nil {
		t.Fatal(err)
	}
	if err := env.svc.CheckGitHubStatus(ctx, localIssue, localIssue.ProjectID, "in_review"); err != nil {
		t.Fatalf("local issue refused: %v", err)
	}
}
