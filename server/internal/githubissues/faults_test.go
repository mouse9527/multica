package githubissues

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// faultOp is one kind of write from Multica to GitHub, as the failure matrix
// drives it.
type faultOp struct {
	name string
	// setup prepares a target of its own for case i, so the cases of one op
	// share an environment without seeing each other's writes.
	setup func(t *testing.T, env *writeEnv, i int) faultTarget
}

// faultTarget runs one write and reads both sides back.
type faultTarget struct {
	// request is the "METHOD path" GitHub receives for the write.
	request string
	// write starts the write. On success the caller finishes it with
	// localErr (nil: the local write succeeds). It returns the error the
	// handler would see; a create carries out the local write itself.
	write func(localErr error) error
	// github and local describe the written thing on each side, for
	// comparing them: "" is absent.
	github, local func() string
	// before and after are what github and local read before the write and
	// once it took effect.
	before, after string
}

var faultOps = []faultOp{
	{
		name: "create issue",
		setup: func(t *testing.T, env *writeEnv, i int) faultTarget {
			title := fmt.Sprintf("Matrix %d", i)
			return faultTarget{
				request: "POST /repositories/42/issues",
				write: func(localErr error) error {
					w := env.beginCreate(t, env.member)
					_, err := env.localIssue(w, env.member, title, "", "backlog", localErr == nil)
					w.Abandon(context.Background(), err)
					return err
				},
				github: func() string {
					env.api.mu.Lock()
					defer env.api.mu.Unlock()
					n := 0
					for _, issue := range env.api.issues {
						if issue["title"] == title {
							n++
						}
					}
					return copies(n)
				},
				local: func() string {
					return copies(env.count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = $2`, env.ws, title))
				},
				before: "", after: copies(1),
			}
		},
	},
	{
		name: "update issue",
		setup: func(t *testing.T, env *writeEnv, i int) faultTarget {
			number := i + 1
			issue := env.importedIssue(t, number)
			title := "Renamed"
			return faultTarget{
				request: fmt.Sprintf("PATCH /repositories/42/issues/%d", number),
				write: func(localErr error) error {
					w, err := env.svc.BeginIssueUpdate(context.Background(), env.member, issue, IssueChange{Title: &title})
					if err != nil {
						return err
					}
					if localErr == nil {
						env.f.Exec(t, `UPDATE issue SET title = $2 WHERE id = $1`, issue.ID, title)
					}
					w.Finish(context.Background(), localErr)
					return localErr
				},
				github: func() string {
					env.api.mu.Lock()
					defer env.api.mu.Unlock()
					return env.api.issues[number]["title"].(string)
				},
				local:  func() string { return env.issue(t, number).title },
				before: "Crash on start", after: title,
			}
		},
	},
	{
		name: "create comment",
		setup: func(t *testing.T, env *writeEnv, i int) faultTarget {
			issue := env.importedIssue(t, 1)
			text := fmt.Sprintf("Matrix comment %d", i)
			return faultTarget{
				request: "POST /repositories/42/issues/1/comments",
				write: func(localErr error) error {
					w, err := env.svc.BeginCommentCreate(context.Background(), env.member, issue, text)
					if err != nil {
						return err
					}
					if localErr == nil {
						env.localComment(t, w, issue, env.member, text)
					} else {
						w.Finish(context.Background(), localErr)
					}
					return localErr
				},
				github: func() string { return copies(env.githubComments(text)) },
				local:  func() string { return copies(env.localComments(t, text)) },
				before: "", after: copies(1),
			}
		},
	},
	{
		name: "update comment",
		setup: func(t *testing.T, env *writeEnv, i int) faultTarget {
			comment := env.postedComment(t, env.importedIssue(t, 1), fmt.Sprintf("v1 of %d", i))
			v2 := fmt.Sprintf("v2 of %d", i)
			return faultTarget{
				request: "PATCH /repositories/42/issues/comments/" + env.githubCommentID(t, comment),
				write: func(localErr error) error {
					w, err := env.svc.BeginCommentUpdate(context.Background(), env.member, comment, v2, CommentBase{})
					if err != nil {
						return err
					}
					if localErr == nil {
						if _, err := db.New(env.svc.db).UpdateComment(context.Background(), db.UpdateCommentParams{ID: comment.ID, Content: v2}); err != nil {
							t.Fatal(err)
						}
					}
					w.Finish(context.Background(), localErr)
					return localErr
				},
				github: func() string {
					return fmt.Sprintf("v1:%d v2:%d", env.githubComments(comment.Content), env.githubComments(v2))
				},
				local: func() string {
					return fmt.Sprintf("v1:%d v2:%d", env.localComments(t, comment.Content), env.localComments(t, v2))
				},
				before: "v1:1 v2:0", after: "v1:0 v2:1",
			}
		},
	},
	{
		name: "delete comment",
		setup: func(t *testing.T, env *writeEnv, i int) faultTarget {
			env.svc.SetCommentDeleter(&fakeDeleter{env: env})
			comment := env.postedComment(t, env.importedIssue(t, 1), fmt.Sprintf("doomed %d", i))
			return faultTarget{
				request: "DELETE /repositories/42/issues/comments/" + env.githubCommentID(t, comment),
				write: func(localErr error) error {
					w, err := env.svc.BeginCommentDelete(context.Background(), env.member, comment)
					if err != nil {
						return err
					}
					if localErr == nil {
						env.f.Exec(t, `DELETE FROM comment WHERE id = $1`, comment.ID)
					}
					w.Finish(context.Background(), localErr)
					return localErr
				},
				github: func() string { return copies(env.githubComments(comment.Content)) },
				local:  func() string { return copies(env.localComments(t, comment.Content)) },
				before: copies(1), after: "",
			}
		},
	},
}

// faultCase is one way a write fails.
type faultCase struct {
	name string
	// fault and applied are the injected failure (failNext); fault "" is a
	// GitHub success followed by a failed local write.
	fault   string
	applied bool
	// kind is the error the handler sees; 0 means the local write's own error.
	kind WriteErrorKind
	// effect is whether the write takes effect in the end, on both sides.
	effect bool
	// state is the final intent state.
	state string
}

var faultCases = []faultCase{
	{"5xx after GitHub wrote it", "500", true, WriteUnknown, true, intentApplied},
	{"5xx, nothing written", "500", false, WriteUnknown, false, intentUnknown},
	{"disconnect after GitHub wrote it", "disconnect", true, WriteUnknown, true, intentApplied},
	{"disconnect, nothing written", "disconnect", false, WriteUnknown, false, intentUnknown},
	{"timeout after GitHub wrote it", "timeout", true, WriteUnknown, true, intentApplied},
	{"timeout, nothing written", "timeout", false, WriteUnknown, false, intentUnknown},
	{"429", "429", false, WriteRateLimited, false, intentFailed},
	{"GitHub ok, local write failed", "", true, 0, true, intentApplied},
}

var errLocalWrite = errors.New("local write failed: database unavailable")

// TestFailureInjectionMatrix runs every write from Multica (new issue, issue
// edit, new comment, comment edit, comment delete) through every failure the
// design names: 5xx, disconnect and timeout, each with GitHub having carried
// out the write or not; 429; and GitHub accepting while the local write
// fails. After two polls and two passes of the grace period, for every pair:
//
//   - GitHub received the write exactly once: nothing is retried;
//   - GitHub and Multica agree, with at most one copy of anything created;
//   - the write took effect exactly when GitHub carried it out;
//   - an unanswered write GitHub never carried out is handed to a person
//     (unknown + needs_confirmation), a throttled one failed.
//
// The tests in write_test.go cover the same paths one by one with more
// detail (authorship, signatures, wakeups); this is the summary.
func TestFailureInjectionMatrix(t *testing.T) {
	for _, op := range faultOps {
		t.Run(op.name, func(t *testing.T) {
			// One environment per op keeps the matrix to a few databases;
			// each case writes to a target of its own.
			env := newWriteEnv(t)
			for i, fc := range faultCases {
				t.Run(fc.name, func(t *testing.T) {
					target := op.setup(t, env, i)
					sentBefore := env.api.sentCount(target.request)
					if got, local := target.github(), target.local(); got != target.before || local != target.before {
						t.Fatalf("before the write: GitHub %q, local %q, want %q", got, local, target.before)
					}

					var localErr error
					if fc.fault == "" {
						localErr = errLocalWrite
					} else {
						env.api.failNext(fc.fault, fc.applied)
					}
					err := target.write(localErr)
					switch {
					case fc.kind != 0:
						wantWriteError(t, err, fc.kind)
					case err == nil:
						t.Fatal("write reported success over a failed local write")
					}
					if got := target.local(); got != target.before {
						t.Fatalf("local right after the failure = %q, want %q", got, target.before)
					}

					env.tick(t)
					env.tick(t)
					env.laterThanGrace(t)
					env.laterThanGrace(t)

					want := target.before
					if fc.effect {
						want = target.after
					}
					if got := target.github(); got != want {
						t.Errorf("GitHub = %q, want %q", got, want)
					}
					if got := target.local(); got != want {
						t.Errorf("local = %q, want %q", got, want)
					}
					if n := env.api.sentCount(target.request) - sentBefore; n != 1 {
						t.Errorf("%s sent %d times, want 1 (never retried)", target.request, n)
					}
					intent := env.lastIntent(t)
					if intent.state != fc.state {
						t.Errorf("intent = %+v, want state %s", intent, fc.state)
					}
					if fc.state == intentUnknown && !strings.HasPrefix(intent.errText, needsConfirmation) {
						t.Errorf("unanswered write intent = %+v, want it handed to a person", intent)
					}
				})
			}
		})
	}
}

// laterThanGrace is pastGrace for cases that share an environment: the clock
// only moves forward, past the grace period of every intent so far, and a
// rate-limit pause of an earlier case has run out.
func (env *writeEnv) laterThanGrace(t *testing.T) PollStats {
	t.Helper()
	from := time.Now().UTC()
	if env.now.After(from) {
		from = *env.now
	}
	*env.now = from.Add(unknownGrace + time.Minute)
	env.api.setNow(*env.now)
	return env.poll(t)
}

// githubComments counts the comments on GitHub whose text, without the
// marker Multica appends, is text.
func (env *writeEnv) githubComments(text string) int {
	n := 0
	for _, body := range env.api.commentBodies() {
		if stripMarker(body) == text {
			n++
		}
	}
	return n
}

func (env *writeEnv) localComments(t *testing.T, text string) int {
	n := 0
	for _, c := range env.comments(t, env.importedIssue1(t)) {
		if c.content == text {
			n++
		}
	}
	return n
}

// importedIssue1 is issue 1, which the comment cases write on.
func (env *writeEnv) importedIssue1(t *testing.T) db.Issue {
	t.Helper()
	issue, err := db.New(env.svc.db).GetIssue(context.Background(), env.uuid(t, env.issue(t, 1).id))
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func (env *writeEnv) githubCommentID(t *testing.T, comment db.Comment) string {
	t.Helper()
	var id int64
	env.f.QueryRow(t, `SELECT external_id FROM comment_external WHERE comment_id = $1`, comment.ID).Scan(&id)
	return fmt.Sprint(id)
}

// copies renders a count of copies for comparison: "" for none.
func copies(n int) string {
	return strings.Repeat("copy;", n)
}
