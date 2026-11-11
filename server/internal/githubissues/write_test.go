package githubissues

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// writeEnv is a pollEnv whose clock is real time (intents are stamped by the
// database clock, and the grace period compares the two) and whose requests
// time out quickly, so a hanging GitHub is a timeout within the test.
type writeEnv struct {
	*pollEnv
	member Actor
	agent  Actor
}

func newWriteEnv(t *testing.T) *writeEnv {
	t.Helper()
	env := &writeEnv{pollEnv: newPollEnv(t)}
	*env.now = time.Now().UTC().Truncate(time.Second)
	env.api.setNow(*env.now)
	env.api.mu.Lock()
	env.api.writes.author = "octocat"
	env.api.mu.Unlock()
	env.svc.repoHTTPClient = noRedirectClient(&http.Client{Timeout: 300 * time.Millisecond})
	env.member = Actor{Type: "member", ID: env.uuid(t, env.owner)}
	runtime := env.f.Runtime(t, "Cloud")
	env.agent = Actor{Type: "agent", ID: env.uuid(t, env.f.Agent(t, "研析", runtime)), Name: "研析"}
	return env
}

func (env *writeEnv) uuid(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	id, err := util.ParseUUID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// tick moves the clock one poll cycle on and polls.
func (env *writeEnv) tick(t *testing.T) PollStats {
	t.Helper()
	*env.now = env.now.Add(pollInterval)
	env.api.setNow(*env.now)
	return env.poll(t)
}

// pastGrace moves the clock past the grace period of every intent so far and
// polls.
func (env *writeEnv) pastGrace(t *testing.T) PollStats {
	t.Helper()
	*env.now = time.Now().UTC().Add(unknownGrace + time.Minute)
	env.api.setNow(*env.now)
	return env.poll(t)
}

// importedIssue is a GitHub issue opened by someone else, imported by a poll.
func (env *writeEnv) importedIssue(t *testing.T, number int) db.Issue {
	t.Helper()
	env.api.putIssue(number, "stranger", "Crash on start", "It crashes.", "open", env.now.Add(-time.Minute), nil)
	env.poll(t)
	issue, err := db.New(env.svc.db).GetIssue(context.Background(), env.uuid(t, env.issue(t, number).id))
	if err != nil {
		t.Fatalf("imported issue %d: %v", number, err)
	}
	return issue
}

// ownIssue is an issue opened on GitHub from Multica, as the create handler
// writes it.
func (env *writeEnv) ownIssue(t *testing.T, actor Actor, title, description string) db.Issue {
	t.Helper()
	w, err := env.svc.BeginIssueCreate(context.Background(), env.uuid(t, env.ws), actor, NewIssue{
		Repo: "acme/widgets", Title: title, Description: description, Status: "backlog", ProjectID: env.uuid(t, env.project),
	})
	if err != nil {
		t.Fatalf("begin create: %v", err)
	}
	issue, err := env.localIssue(w, actor, title, description)
	if err != nil {
		t.Fatalf("local create: %v", err)
	}
	return issue
}

// localIssue is the create transaction of IssueService.Create, reduced to
// what InTx needs.
func (env *writeEnv) localIssue(w *Write, actor Actor, title, description string) (db.Issue, error) {
	ctx := context.Background()
	tx, err := env.svc.db.Begin(ctx)
	if err != nil {
		return db.Issue{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	ws, _ := util.ParseUUID(env.ws)
	number, err := service.AllocateIssueNumber(ctx, q, ws, service.IssueCountPolicy{Action: entitlement.ActionOff})
	if err != nil {
		return db.Issue{}, err
	}
	project, _ := util.ParseUUID(env.project)
	issue, err := q.CreateIssue(ctx, db.CreateIssueParams{
		ID: dbid.NewV7(), WorkspaceID: ws, Title: title, Description: pgtype.Text{String: description, Valid: true},
		Status: "backlog", Priority: "none", CreatorType: actor.Type, CreatorID: actor.ID, Number: number, ProjectID: project,
	})
	if err != nil {
		return db.Issue{}, err
	}
	if err := w.InTx(ctx, q, issue); err != nil {
		return db.Issue{}, err
	}
	return issue, tx.Commit(ctx)
}

// localComment is what the comment handler writes after GitHub accepted.
func (env *writeEnv) localComment(t *testing.T, w *Write, issue db.Issue, actor Actor, content string) {
	t.Helper()
	_, err := db.New(env.svc.db).CreateComment(context.Background(), db.CreateCommentParams{
		ID: w.CommentID(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: actor.Type, AuthorID: actor.ID, Content: content, Type: "comment",
	})
	w.Finish(context.Background(), err)
	if err != nil {
		t.Fatalf("local comment: %v", err)
	}
}

type intentRow struct{ op, state, errText string }

func (env *writeEnv) lastIntent(t *testing.T) intentRow {
	t.Helper()
	var got intentRow
	env.f.QueryRow(t, `SELECT op, state, COALESCE(error, '') FROM github_write_intent
		WHERE workspace_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, env.ws).Scan(&got.op, &got.state, &got.errText)
	return got
}

type commentRow struct{ id, authorType, authorID, content string }

func (env *writeEnv) comments(t *testing.T, issue db.Issue) []commentRow {
	t.Helper()
	rows, err := env.svc.db.Query(context.Background(), `SELECT id::text, author_type, author_id::text, content
		FROM comment WHERE issue_id = $1 AND deleted_at IS NULL ORDER BY created_at, id`, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []commentRow
	for rows.Next() {
		var c commentRow
		if err := rows.Scan(&c.id, &c.authorType, &c.authorID, &c.content); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func wantWriteError(t *testing.T, err error, kind WriteErrorKind) {
	t.Helper()
	var writeErr *WriteError
	if !errors.As(err, &writeErr) || writeErr.Kind != kind {
		t.Fatalf("error = %v, want WriteError kind %d", err, kind)
	}
}

// A comment written from Multica reaches GitHub with the agent's signature
// and marker, is linked by GitHub id, and its echo in the next polls changes
// nothing and wakes nobody.
func TestWriteCommentSignsLinksAndEchoesSilently(t *testing.T) {
	env := newWriteEnv(t)
	issue := env.importedIssue(t, 1)
	ctx := context.Background()

	w, err := env.svc.BeginCommentCreate(ctx, env.agent, issue, "Fixed in #2.")
	if err != nil || w == nil {
		t.Fatalf("begin comment: %v %v", w, err)
	}
	env.localComment(t, w, issue, env.agent, "Fixed in #2.")

	bodies := env.api.commentBodies()
	op := w.intent.ID
	want := "Fixed in #2.\n\n— 研析（智能体），经 Multica\n<!-- multica:v1 actor=agent:" +
		util.UUIDToString(env.agent.ID) + " op=" + util.UUIDToString(op) + " -->"
	if len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("GitHub comment bodies = %q, want %q", bodies, want)
	}
	if got := env.lastIntent(t); got.op != opCreateComment || got.state != intentApplied {
		t.Fatalf("intent = %+v", got)
	}
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE comment_id = $1`, w.CommentID()); n != 1 {
		t.Fatalf("comment_external rows = %d", n)
	}
	// The agent reacts to comments on the issue: its own echo must not wake it.
	wakeupID := env.f.Insert(t, "issue_wakeup", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": env.ws, "issue_id": util.UUIDToString(issue.ID),
		"agent_id": util.UUIDToString(env.agent.ID), "instruction": "react to comments", "kind": "event",
		"mode": "continuous", "event_types": []string{"comment.created", "comment.updated"},
	})
	receipts := func() int {
		return env.count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND processed_at IS NULL`, wakeupID)
	}

	for range 2 {
		env.tick(t)
	}
	got := env.comments(t, issue)
	if len(got) != 1 || got[0].authorType != "agent" || got[0].content != "Fixed in #2." {
		t.Fatalf("comments after echo = %+v", got)
	}
	// The token owner edits the comment on github.com: GitHub's text wins,
	// the author stays the agent, and nobody is woken.
	env.api.mu.Lock()
	for _, c := range env.api.comments {
		c["body"] = strings.Replace(c["body"].(string), "Fixed in #2.", "Fixed in #3.", 1)
		c["updated_at"] = env.now.Add(time.Second).UTC().Format(time.RFC3339)
	}
	env.api.mu.Unlock()
	env.tick(t)
	got = env.comments(t, issue)
	if len(got) != 1 || got[0].authorType != "agent" || got[0].content != "Fixed in #3." {
		t.Fatalf("comments after GitHub edit = %+v", got)
	}
	if n := receipts(); n != 0 {
		t.Fatalf("echo and edit left %d wakeup receipts", n)
	}

	// A member's comment carries the marker only.
	w2, err := env.svc.BeginCommentCreate(ctx, env.member, issue, "Thanks")
	if err != nil {
		t.Fatal(err)
	}
	env.localComment(t, w2, issue, env.member, "Thanks")
	if body := env.api.lastBody()["body"].(string); strings.Contains(body, "（智能体）") ||
		!strings.HasSuffix(body, "op="+util.UUIDToString(w2.intent.ID)+" -->") {
		t.Fatalf("member comment body = %q", body)
	}
}

// Failure injection on a new comment: whatever GitHub does, Multica ends up
// with at most one copy, never re-sends the write, and hands an unanswered
// write that never shows up to a person after the grace period.
func TestWriteCommentFailureInjection(t *testing.T) {
	cases := []struct {
		name      string
		fault     string
		applied   bool
		kind      WriteErrorKind
		state     string
		wantLocal int
	}{
		{"5xx after GitHub wrote it", "500", true, WriteUnknown, intentUnknown, 1},
		{"disconnect after GitHub wrote it", "disconnect", true, WriteUnknown, intentUnknown, 1},
		{"timeout after GitHub wrote it", "timeout", true, WriteUnknown, intentUnknown, 1},
		{"5xx, nothing written", "500", false, WriteUnknown, intentUnknown, 0},
		{"timeout, nothing written", "timeout", false, WriteUnknown, intentUnknown, 0},
		{"429", "429", false, WriteRateLimited, intentFailed, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newWriteEnv(t)
			issue := env.importedIssue(t, 1)
			env.api.failNext(tc.fault, tc.applied)

			w, err := env.svc.BeginCommentCreate(context.Background(), env.member, issue, "Looking into it")
			if w != nil {
				t.Fatalf("write returned for a failed request")
			}
			wantWriteError(t, err, tc.kind)
			if got := env.lastIntent(t); got.state != tc.state {
				t.Fatalf("intent after failure = %+v, want %s", got, tc.state)
			}
			if n := len(env.comments(t, issue)); n != 0 {
				t.Fatalf("local comments right after failure = %d", n)
			}
			if tc.kind == WriteRateLimited {
				// Throttled polling pauses too; nothing more to see.
				return
			}

			env.tick(t)
			got := env.comments(t, issue)
			if len(got) != tc.wantLocal {
				t.Fatalf("local comments after poll = %+v, want %d", got, tc.wantLocal)
			}
			if tc.wantLocal == 1 && (got[0].authorType != "member" || got[0].authorID != env.owner || got[0].content != "Looking into it") {
				t.Fatalf("adopted comment = %+v", got[0])
			}

			env.pastGrace(t)
			intent := env.lastIntent(t)
			if tc.applied {
				if intent.state != intentApplied {
					t.Fatalf("adopted write intent = %+v", intent)
				}
			} else if intent.state != intentUnknown || !strings.HasPrefix(intent.errText, needsConfirmation) {
				t.Fatalf("unanswered write intent = %+v, want unknown + needs_confirmation", intent)
			}
			env.pastGrace(t)
			if n := env.api.sentCount("POST /repositories/42/issues/1/comments"); n != 1 {
				t.Fatalf("comment POSTs = %d, want 1 (never retried)", n)
			}
			if n := len(env.api.commentBodies()); n != tc.wantLocal {
				t.Fatalf("GitHub comments = %d, want %d", n, tc.wantLocal)
			}
			if n := len(env.comments(t, issue)); n != tc.wantLocal {
				t.Fatalf("local comments at the end = %d, want %d", n, tc.wantLocal)
			}
		})
	}
}

// GitHub accepted a comment but the local insert failed: the next poll heals
// it under its Multica author, once.
func TestWriteCommentLocalFailureHealsOnPoll(t *testing.T) {
	env := newWriteEnv(t)
	issue := env.importedIssue(t, 1)
	w, err := env.svc.BeginCommentCreate(context.Background(), env.agent, issue, "Done")
	if err != nil {
		t.Fatal(err)
	}
	w.Finish(context.Background(), errors.New("database unavailable"))
	if got := env.lastIntent(t); got.state != intentGitHubOK || !strings.Contains(got.errText, "local write failed") {
		t.Fatalf("intent = %+v", got)
	}
	env.tick(t)
	env.tick(t)
	got := env.comments(t, issue)
	if len(got) != 1 || got[0].id != util.UUIDToString(w.CommentID()) || got[0].authorType != "agent" || got[0].content != "Done" {
		t.Fatalf("healed comments = %+v", got)
	}
	if got := env.lastIntent(t); got.state != intentApplied {
		t.Fatalf("intent after heal = %+v", got)
	}
}

// Issues opened from Multica: linked in the create transaction, echoed
// without a second copy, and healed exactly once when the local create
// failed or GitHub's answer was lost.
func TestWriteIssueCreateNeverDuplicates(t *testing.T) {
	env := newWriteEnv(t)
	ctx := context.Background()
	ws := env.uuid(t, env.ws)
	countTitle := func(title string) int {
		return env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = $2`, env.ws, title)
	}

	issue := env.ownIssue(t, env.agent, "Ship it", "Plan:\n- a")
	body := env.api.lastBody()
	if body["title"] != "Ship it" || !strings.HasPrefix(body["body"].(string), "Plan:\n- a\n\n— 研析（智能体），经 Multica\n<!-- multica:v1 actor=agent:") {
		t.Fatalf("create body = %v", body)
	}
	env.tick(t)
	env.tick(t)
	if countTitle("Ship it") != 1 {
		t.Fatalf("echo duplicated the issue")
	}
	var description, creatorType string
	env.f.QueryRow(t, `SELECT description, creator_type FROM issue WHERE id = $1`, issue.ID).Scan(&description, &creatorType)
	if description != "Plan:\n- a" || creatorType != "agent" {
		t.Fatalf("issue after echo = %q %s", description, creatorType)
	}

	// The local create fails after GitHub opened the issue.
	w, err := env.svc.BeginIssueCreate(ctx, ws, env.agent, NewIssue{Repo: "acme/widgets", Title: "Abandoned", Status: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	w.Abandon(ctx, errors.New("duplicate guard"))
	// GitHub opens it but the answer is lost.
	env.api.failNext("500", true)
	_, err = env.svc.BeginIssueCreate(ctx, ws, env.member, NewIssue{Repo: "acme/widgets", Title: "Lost answer", Status: "backlog"})
	wantWriteError(t, err, WriteUnknown)
	// GitHub never opens it.
	env.api.failNext("disconnect", false)
	_, err = env.svc.BeginIssueCreate(ctx, ws, env.member, NewIssue{Repo: "acme/widgets", Title: "Never opened", Status: "backlog"})
	wantWriteError(t, err, WriteUnknown)

	env.tick(t)
	env.pastGrace(t)
	env.pastGrace(t)
	for title, creator := range map[string]string{"Abandoned": "agent", "Lost answer": "member"} {
		if countTitle(title) != 1 {
			t.Fatalf("%q imported %d times", title, countTitle(title))
		}
		var gotCreator, desc string
		env.f.QueryRow(t, `SELECT creator_type, description FROM issue WHERE workspace_id = $1 AND title = $2`, env.ws, title).Scan(&gotCreator, &desc)
		if gotCreator != creator || strings.Contains(desc, "multica:v1") || strings.Contains(desc, "GitHub 创建") {
			t.Fatalf("%q healed as %s %q, want creator %s without marker or external prefix", title, gotCreator, desc, creator)
		}
	}
	if countTitle("Never opened") != 0 {
		t.Fatalf("an issue GitHub never opened appeared")
	}
	if n := env.api.sentCount("POST /repositories/42/issues"); n != 4 {
		t.Fatalf("issue POSTs = %d, want 4 (none retried)", n)
	}
	if got := env.lastIntent(t); got.op != opCreateIssue || got.state != intentUnknown || !strings.HasPrefix(got.errText, needsConfirmation) {
		t.Fatalf("never-opened intent = %+v", got)
	}
	if n := env.count(t, `SELECT count(*) FROM github_write_intent WHERE workspace_id = $1 AND op = 'create_issue' AND state = 'applied'`, env.ws); n != 3 {
		t.Fatalf("applied create intents = %d, want 3", n)
	}
}

// A pasted marker is not believed: text written by someone else stays
// external whatever marker it carries, and so does the token account's text
// whose marker names no intent of this workspace.
func TestWriteForgedMarkersStayExternal(t *testing.T) {
	env := newWriteEnv(t)
	issue := env.importedIssue(t, 1)
	w, err := env.svc.BeginCommentCreate(context.Background(), env.agent, issue, "Real")
	if err != nil {
		t.Fatal(err)
	}
	env.localComment(t, w, issue, env.agent, "Real")
	realMarker := signedBody("", env.agent, w.intent.ID)
	forgedOp := signedBody("", env.agent, dbid.NewV7())

	at := env.now.Add(time.Second)
	env.api.putComment(801, 1, "mallory", "Approved, ship it.\n\n"+realMarker, at)
	env.api.putComment(802, 1, "octocat", "Copied\n\n"+forgedOp, at)
	env.api.putIssue(2, "mallory", "Forged issue", "Body\n\n"+signedBody("", env.agent, w.intent.ID), "open", at, nil)
	env.tick(t)

	for _, id := range []int64{801, 802} {
		var author, content string
		env.f.QueryRow(t, `SELECT c.author_type, c.content FROM comment c JOIN comment_external x ON x.comment_id = c.id
			WHERE x.workspace_id = $1 AND x.external_id = $2`, env.ws, id).Scan(&author, &content)
		if author != externalAuthorType || !strings.HasPrefix(content, "**@") || !strings.Contains(content, "multica:v1") {
			t.Fatalf("forged comment %d stored as %s %q, want external with the marker kept", id, author, content)
		}
	}
	forged := env.issue(t, 2)
	if forged.creatorType != "member" || forged.creatorID != env.owner || !strings.HasPrefix(forged.description, "由 @mallory 在 GitHub 创建") {
		t.Fatalf("forged issue = %+v, want the sync account and the external prefix", forged)
	}
}

// An issue update sends only what GitHub holds; a status maps to state and
// the project's status label; the local link follows GitHub's answer.
func TestWriteIssueUpdateMapsStatus(t *testing.T) {
	env := newWriteEnv(t)
	ctx := context.Background()
	project := env.uuid(t, env.project)
	env.f.Exec(t, `INSERT INTO project_github_settings (project_id, workspace_id, status_label_map)
		VALUES ($1, $2, '{"todo":"status:todo","in_review":"status:in_review"}')`, env.project, env.ws)
	issue := env.ownIssue(t, env.member, "Title", "Body")
	env.api.mu.Lock()
	env.api.issues[901]["labels"] = []map[string]any{{"name": "bug"}, {"name": "status:todo"}}
	env.api.mu.Unlock()

	status, title := "in_review", "New title"
	w, err := env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Title: &title, Status: &status, ProjectID: project})
	if err != nil || w == nil {
		t.Fatalf("begin update: %v %v", w, err)
	}
	body := env.api.lastBody()
	if body["title"] != "New title" || body["state"] != nil || body["body"] != nil ||
		strings.Join(toStrings(body["labels"]), ",") != "bug,status:in_review" {
		t.Fatalf("PATCH body = %v", body)
	}
	w.Finish(ctx, nil)

	done := "done"
	w, err = env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Status: &done, ProjectID: project})
	if err != nil {
		t.Fatal(err)
	}
	// Closing also drops the open status label, which no longer applies.
	if body := env.api.lastBody(); body["state"] != "closed" || body["state_reason"] != "completed" ||
		strings.Join(toStrings(body["labels"]), ",") != "bug" {
		t.Fatalf("close body = %v", body)
	}
	w.Finish(ctx, nil)
	if got := env.issue(t, 901); got.state != "closed" {
		t.Fatalf("issue_external state = %s", got.state)
	}

	// Multica-only fields send nothing.
	if w, err := env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Title: &issue.Title, ProjectID: project}); w != nil || err != nil {
		t.Fatalf("unchanged update sent a write: %v %v", w, err)
	}
	// A local issue sends nothing.
	local, _ := db.New(env.svc.db).GetIssue(ctx, env.uuid(t, env.f.Issue(t, "local")))
	if w, err := env.svc.BeginIssueUpdate(ctx, env.member, local, IssueChange{Title: &title}); w != nil || err != nil {
		t.Fatalf("local issue sent a write: %v %v", w, err)
	}
}

// An update whose answer was lost is confirmed by comparing GitHub's current
// fields with what was sent, or handed to a person; it is never re-sent.
func TestWriteIssueUpdateUnknownIsConfirmedOrHandedOver(t *testing.T) {
	env := newWriteEnv(t)
	ctx := context.Background()
	issue := env.importedIssue(t, 1)

	applied := "Renamed on GitHub"
	env.api.failNext("timeout", true)
	_, err := env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Title: &applied})
	wantWriteError(t, err, WriteUnknown)
	env.pastGrace(t)
	if got := env.lastIntent(t); got.state != intentApplied {
		t.Fatalf("applied-but-unanswered update = %+v", got)
	}
	if got := env.issue(t, 1); got.title != applied {
		t.Fatalf("local title = %q, want GitHub's", got.title)
	}

	lost := "Never renamed"
	env.api.failNext("500", false)
	_, err = env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Title: &lost})
	wantWriteError(t, err, WriteUnknown)
	env.pastGrace(t)
	if got := env.lastIntent(t); got.state != intentUnknown || !strings.HasPrefix(got.errText, needsConfirmation) {
		t.Fatalf("lost update = %+v", got)
	}
	if n := env.api.sentCount("PATCH /repositories/42/issues/1"); n != 2 {
		t.Fatalf("PATCHes = %d, want 2", n)
	}
}

// Rejections and refusals write nothing locally and leave a failed intent;
// the description of an issue opened on GitHub by someone else is read-only.
func TestWriteRejectionsAndReadOnlyDescription(t *testing.T) {
	env := newWriteEnv(t)
	ctx := context.Background()
	issue := env.importedIssue(t, 1)

	desc := "rewritten"
	_, err := env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Description: &desc})
	wantWriteError(t, err, WriteNotAllowed)

	env.f.Exec(t, `DELETE FROM project_resource WHERE workspace_id = $1`, env.ws)
	title := "x"
	_, err = env.svc.BeginIssueUpdate(ctx, env.member, issue, IssueChange{Title: &title})
	wantWriteError(t, err, WriteUnavailable)

	_, err = env.svc.BeginIssueCreate(ctx, env.uuid(t, env.ws), env.member, NewIssue{Repo: "acme/widgets", Title: "x", Status: "done"})
	wantWriteError(t, err, WriteNotAllowed)
	// A status GitHub cannot show is refused like on the update path.
	var unmapped *UnmappedStatusError
	if _, err = env.svc.BeginIssueCreate(ctx, env.uuid(t, env.ws), env.member, NewIssue{Repo: "acme/widgets", Title: "x", Status: "in_progress"}); !errors.As(err, &unmapped) {
		t.Fatalf("unmapped status on create = %v", err)
	}
	if n := env.count(t, `SELECT count(*) FROM github_write_intent WHERE workspace_id = $1`, env.ws); n != 0 {
		t.Fatalf("refused writes left %d intents", n)
	}
}

// Editing and deleting a comment Multica posted go to GitHub; a comment
// GitHub no longer has counts as deleted; a local-only comment sends nothing.
func TestWriteCommentEditAndDelete(t *testing.T) {
	env := newWriteEnv(t)
	ctx := context.Background()
	issue := env.importedIssue(t, 1)
	w, _ := env.svc.BeginCommentCreate(ctx, env.member, issue, "v1")
	env.localComment(t, w, issue, env.member, "v1")
	comment, err := db.New(env.svc.db).GetComment(ctx, w.CommentID())
	if err != nil {
		t.Fatal(err)
	}

	edit, err := env.svc.BeginCommentUpdate(ctx, env.member, comment, "v2")
	if err != nil || edit == nil {
		t.Fatalf("begin edit: %v %v", edit, err)
	}
	if _, err := db.New(env.svc.db).UpdateComment(ctx, db.UpdateCommentParams{ID: comment.ID, Content: "v2"}); err != nil {
		t.Fatal(err)
	}
	edit.Finish(ctx, nil)
	if bodies := env.api.commentBodies(); len(bodies) != 1 || !strings.HasPrefix(bodies[0], "v2\n\n<!-- multica:v1 actor=member:") {
		t.Fatalf("GitHub after edit = %q", bodies)
	}
	env.tick(t)
	if got := env.comments(t, issue); len(got) != 1 || got[0].content != "v2" {
		t.Fatalf("local after edit echo = %+v", got)
	}

	// Someone deletes it on GitHub first; Multica's delete still succeeds.
	env.api.mu.Lock()
	for id := range env.api.comments {
		delete(env.api.comments, id)
	}
	env.api.mu.Unlock()
	comment, _ = db.New(env.svc.db).GetComment(ctx, comment.ID)
	del, err := env.svc.BeginCommentDelete(ctx, env.member, comment)
	if err != nil || del == nil {
		t.Fatalf("delete of a comment GitHub no longer has: %v %v", del, err)
	}
	del.Finish(ctx, nil)
	if n := env.count(t, `SELECT count(*) FROM comment_external WHERE comment_id = $1`, comment.ID); n != 0 {
		t.Fatalf("comment_external kept after delete")
	}

	local, _ := db.New(env.svc.db).GetComment(ctx, env.uuid(t, env.f.Comment(t, util.UUIDToString(issue.ID), "local only")))
	if w, err := env.svc.BeginCommentUpdate(ctx, env.member, local, "changed"); w != nil || err != nil {
		t.Fatalf("local-only comment sent a write: %v %v", w, err)
	}
}

func toStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}
