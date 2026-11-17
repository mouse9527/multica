// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package main

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestInboxResolveReconcileAfterARealHandoff drives a workflow handoff away
// from the owner, outside a waiting status, through the upstream activity
// listener, with the inbox resolve listener skipped or failing, and checks
// that reconcile archives the owner's item from the activity row it wrote.
func TestInboxResolveReconcileAfterARealHandoff(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	cfg := agentrouter.InboxResolveConfig{OwnerUserID: testUserID, WaitingStatuses: []string{"started_2"}}
	if err := agentrouter.SetInboxResolveConfig(ctx, testPool, testWorkspaceID, &cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - $2 WHERE id = $1`,
			testWorkspaceID, agentrouter.WorkspaceSettingInboxResolve)
	})

	for name, failing := range map[string]bool{"listener skipped": false, "listener failing": true} {
		t.Run(name, func(t *testing.T) {
			queries := db.New(testPool)
			bus := events.New()
			registerActivityListeners(bus, queries)
			if failing {
				broken := agentrouter.NewInboxResolver(testPool, bus)
				broken.Timeout = time.Nanosecond
				broken.Subscribe(bus)
			}

			issueID := createTestIssue(t, testWorkspaceID, testUserID)
			t.Cleanup(func() {
				cleanupActivities(t, issueID)
				_, _ = testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
				cleanupTestIssue(t, issueID)
			})
			agentID := "0f0e0d0c-0b0a-4909-8807-060504030201"
			var itemID string
			if err := testPool.QueryRow(ctx, `
				INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, severity, title, issue_id, created_at)
				VALUES ($1, 'member', $2, 'issue_assigned', 'action_required', 'assigned', $3, now() - interval '1 hour')
				RETURNING id`, testWorkspaceID, testUserID, issueID).Scan(&itemID); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'in_progress', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`,
				issueID, agentID); err != nil {
				t.Fatal(err)
			}

			agentType, memberType, owner := "agent", "member", testUserID
			bus.Publish(events.Event{
				Type: protocol.EventIssueUpdated, WorkspaceID: testWorkspaceID, ActorType: "member", ActorID: testUserID,
				Payload: map[string]any{
					"issue": handler.IssueResponse{
						ID: issueID, WorkspaceID: testWorkspaceID, Title: "handoff", Status: "in_progress", Priority: "medium",
						CreatorType: "member", CreatorID: testUserID, AssigneeType: &agentType, AssigneeID: &agentID,
					},
					"status_changed": true, "prev_status": "todo",
					"assignee_changed": true, "prev_assignee_type": &memberType, "prev_assignee_id": &owner,
					"workflow_handoff": map[string]string{"workflow": "Delivery", "to_type": "agent", "to_id": agentID, "run": "true"},
				},
			})

			r := agentrouter.NewInboxResolver(testPool, nil)
			r.Grace = 0
			res, err := r.ReconcileWorkspace(ctx, testWorkspaceID)
			if err != nil {
				t.Fatal(err)
			}
			var archived bool
			if err := testPool.QueryRow(ctx, `SELECT archived FROM inbox_item WHERE id = $1`, itemID).Scan(&archived); err != nil {
				t.Fatal(err)
			}
			if !archived || res.Archived < 1 {
				t.Fatalf("archived = %v, reconcile = %+v; want the owner's item archived from the handoff row", archived, res)
			}
		})
	}
}
