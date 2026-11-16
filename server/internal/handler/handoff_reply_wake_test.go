// Modified for the mouse9527/multica fork: handoff reply wake (M-89).

package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// handoffWakeFixture is an issue assigned to agent assignee, with one comment
// by a second agent (reviewer), one by the assignee and one by a member.
type handoffWakeFixture struct {
	assignee, reviewer, issueID                        string
	reviewerComment, assigneeComment, memberComment db.Comment
}

func newHandoffWakeFixture(t *testing.T) handoffWakeFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtime := dbfx.Runtime(t, "handoff-wake-runtime")
	f := handoffWakeFixture{
		assignee: dbfx.Agent(t, "handoff-wake-assignee", runtime),
		reviewer: dbfx.Agent(t, "handoff-wake-reviewer", runtime),
	}
	f.issueID = dbfx.Issue(t, "handoff reply wake", testutil.Cols{
		"assignee_type": "agent", "assignee_id": f.assignee, "status": issuestatus.InReview,
	})
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
	})
	load := func(id string) db.Comment {
		t.Helper()
		c, err := testHandler.Queries.GetComment(ctx, parseUUID(id))
		if err != nil {
			t.Fatalf("load comment: %v", err)
		}
		return c
	}
	f.reviewerComment = load(dbfx.Comment(t, f.issueID, "review done, needs your call",
		testutil.Cols{"author_type": "agent", "author_id": f.reviewer}))
	f.assigneeComment = load(dbfx.Comment(t, f.issueID, "decision card",
		testutil.Cols{"author_type": "agent", "author_id": f.assignee}))
	f.memberComment = load(dbfx.Comment(t, f.issueID, "a note to a colleague"))
	return f
}

// issueIn moves the fixture issue to status and returns it as the handler
// would load it.
func (f handoffWakeFixture) issueIn(t *testing.T, status string) db.Issue {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, f.issueID, status)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(f.issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	return issue
}

// triggeredAgents maps agent id to the source that triggered it, failing on a
// duplicate agent.
func triggeredAgents(t *testing.T, triggers []commentAgentTrigger) map[string]commentAgentTriggerSource {
	t.Helper()
	got := make(map[string]commentAgentTriggerSource, len(triggers))
	for _, tr := range triggers {
		id := uuidToString(tr.Agent.ID)
		if _, dup := got[id]; dup {
			t.Fatalf("agent %s triggered twice: %+v", id, triggers)
		}
		got[id] = tr.Source
	}
	return got
}

func TestHandoffReplyWake(t *testing.T) {
	f := newHandoffWakeFixture(t)
	ctx := context.Background()
	compute := func(issue db.Issue, content string, parent *db.Comment, actorType, actorID string) map[string]commentAgentTriggerSource {
		t.Helper()
		triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, content, parent, actorType, actorID, commentTriggerComputeOptions{})
		return triggeredAgents(t, triggers)
	}

	t.Run("flag on", func(t *testing.T) {
		withFeatureFlag(t, testHandler, featureflags.HandoffReplyWake, true)

		t.Run("reply in another agent's thread wakes the parent author and the assignee", func(t *testing.T) {
			got := compute(f.issueIn(t, issuestatus.InReview), "approved", &f.reviewerComment, "member", testUserID)
			if len(got) != 2 || got[f.reviewer] != commentTriggerSourceThreadParent || got[f.assignee] != commentTriggerSourceIssueAssignee {
				t.Fatalf("triggers = %v, want reviewer (thread parent) and assignee", got)
			}
		})

		t.Run("custom started status waits on a person too", func(t *testing.T) {
			status := customStartedStatus(t)
			got := compute(f.issueIn(t, status), "approved", &f.reviewerComment, "member", testUserID)
			if got[f.assignee] != commentTriggerSourceIssueAssignee {
				t.Fatalf("triggers = %v, want the assignee woken in %q", got, status)
			}
		})

		t.Run("reply in a member thread wakes the assignee", func(t *testing.T) {
			got := compute(f.issueIn(t, issuestatus.InReview), "agreed", &f.memberComment, "member", testUserID)
			if len(got) != 1 || got[f.assignee] != commentTriggerSourceIssueAssignee {
				t.Fatalf("triggers = %v, want only the assignee", got)
			}
		})

		t.Run("explicit mention of another agent also wakes the assignee", func(t *testing.T) {
			content := fmt.Sprintf("[@reviewer](mention://agent/%s) see above", f.reviewer)
			got := compute(f.issueIn(t, issuestatus.InReview), content, &f.memberComment, "member", testUserID)
			if len(got) != 2 || got[f.reviewer] != commentTriggerSourceMentionAgent || got[f.assignee] != commentTriggerSourceIssueAssignee {
				t.Fatalf("triggers = %v, want the mentioned reviewer and the assignee", got)
			}
		})

		t.Run("assignee that wrote the parent is queued once", func(t *testing.T) {
			issue := f.issueIn(t, issuestatus.InReview)
			got := compute(issue, "approved", &f.assigneeComment, "member", testUserID)
			if len(got) != 1 || got[f.assignee] != commentTriggerSourceThreadParent {
				t.Fatalf("triggers = %v, want the assignee once, as thread parent", got)
			}

			reply := dbfx.Comment(t, f.issueID, "approved", testutil.Cols{"parent_id": uuidToString(f.assigneeComment.ID)})
			triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, "approved", &f.assigneeComment, "member", testUserID,
				commentTriggerComputeOptions{ExcludeTriggerCommentID: parseUUID(reply)})
			testHandler.enqueueCommentAgentTriggers(ctx, issue, parseUUID(reply), triggers)
			var queued int
			if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`,
				f.issueID, f.assignee).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued != 1 {
				t.Fatalf("assignee tasks = %d, want 1", queued)
			}
		})

		t.Run("not a handoff status: upstream routing only", func(t *testing.T) {
			for _, status := range []string{issuestatus.InProgress, issuestatus.Todo} {
				got := compute(f.issueIn(t, status), "approved", &f.reviewerComment, "member", testUserID)
				if len(got) != 1 || got[f.reviewer] != commentTriggerSourceThreadParent {
					t.Fatalf("%s: triggers = %v, want only the reviewer", status, got)
				}
			}
		})

		t.Run("agent comments never take the route", func(t *testing.T) {
			got := compute(f.issueIn(t, issuestatus.InReview), "done", &f.memberComment, "agent", f.reviewer)
			if _, ok := got[f.assignee]; ok {
				t.Fatalf("triggers = %v, an agent comment must not wake the assignee", got)
			}
		})

		t.Run("note comments stay silent", func(t *testing.T) {
			got := compute(f.issueIn(t, issuestatus.InReview), noteCommentPrefix+" for the record", &f.reviewerComment, "member", testUserID)
			if len(got) != 0 {
				t.Fatalf("triggers = %v, want none for a note", got)
			}
		})
	})

	t.Run("flag off matches upstream", func(t *testing.T) {
		withFeatureFlag(t, testHandler, featureflags.HandoffReplyWake, false)
		issue := f.issueIn(t, issuestatus.InReview)
		for _, parent := range []*db.Comment{&f.reviewerComment, &f.memberComment, &f.assigneeComment, nil} {
			wrapped, _ := testHandler.computeCommentAgentTriggers(ctx, issue, "approved", parent, "member", testUserID, commentTriggerComputeOptions{})
			upstream, _ := testHandler.computeUpstreamCommentAgentTriggers(ctx, issue, "approved", parent, "member", testUserID, commentTriggerComputeOptions{})
			if fmt.Sprint(triggeredAgents(t, wrapped)) != fmt.Sprint(triggeredAgents(t, upstream)) {
				t.Fatalf("flag off: wrapped %v != upstream %v", triggeredAgents(t, wrapped), triggeredAgents(t, upstream))
			}
		}
		got := compute(issue, "approved", &f.reviewerComment, "member", testUserID)
		if len(got) != 1 || got[f.reviewer] != commentTriggerSourceThreadParent {
			t.Fatalf("flag off: triggers = %v, want only the reviewer", got)
		}
	})
}

// customStartedStatus creates a workspace status in the started category, the
// shape of a "waiting for your decision" column.
func customStartedStatus(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("awaiting_decision_%d", time.Now().UnixNano()%1_000_000)
	entry, err := testHandler.Queries.CreateIssueStatusEntry(ctx, db.CreateIssueStatusEntryParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		Key:         key,
		Name:        key,
		Category:    issuestatus.CategoryStarted,
		Color:       "#123456",
	})
	if err != nil {
		t.Fatalf("create custom status: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE id = $1`, entry.ID)
	})
	return entry.Key
}
