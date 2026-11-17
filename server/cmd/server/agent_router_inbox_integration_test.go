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

	type scenario struct {
		// itemType is the owner's item on the issue: issue_assigned when someone
		// else assigned it to the owner, an ordinary comment item when the owner
		// created and assigned it to themselves and has no issue_assigned item.
		itemType string
		failing  bool
	}
	for name, sc := range map[string]scenario{
		"assigned by another, listener skipped": {itemType: "issue_assigned"},
		"assigned by another, listener failing": {itemType: "issue_assigned", failing: true},
		"self-assigned, listener skipped":       {itemType: "new_comment"},
		"self-assigned, listener failing":       {itemType: "new_comment", failing: true},
	} {
		t.Run(name, func(t *testing.T) {
			agentrouter.SetHandoffAnnotation(true)
			t.Cleanup(func() { agentrouter.SetHandoffAnnotation(false) })
			queries := db.New(testPool)
			bus := events.New()
			registerActivityListeners(bus, queries)
			if sc.failing {
				broken := agentrouter.NewInboxResolver(testPool, bus)
				broken.Timeout = time.Nanosecond
				broken.Subscribe(bus)
			}

			// The issue was created assigned to the owner: no activity row
			// names that assignee.
			issueID := createTestIssue(t, testWorkspaceID, testUserID)
			t.Cleanup(func() {
				cleanupActivities(t, issueID)
				_, _ = testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
				cleanupTestIssue(t, issueID)
			})
			itemID := ownerItem(t, issueID, sc.itemType)
			handOff(t, bus, issueID)

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

	// With inbox resolve off the handoff row is exactly what upstream writes.
	t.Run("annotation off", func(t *testing.T) {
		queries := db.New(testPool)
		bus := events.New()
		registerActivityListeners(bus, queries)
		issueID := createTestIssue(t, testWorkspaceID, testUserID)
		t.Cleanup(func() {
			cleanupActivities(t, issueID)
			cleanupTestIssue(t, issueID)
		})
		handOff(t, bus, issueID)
		var hasFrom bool
		if err := testPool.QueryRow(ctx, `SELECT details ? 'handoff_from_type' FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`,
			issueID).Scan(&hasFrom); err != nil || hasFrom {
			t.Fatalf("handoff row has handoff_from_type = %v (%v), want upstream's row", hasFrom, err)
		}
	})
}

const handoffAgentID = "0f0e0d0c-0b0a-4909-8807-060504030201"

func ownerItem(t *testing.T, issueID, itemType string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, severity, title, issue_id, created_at)
		VALUES ($1, 'member', $2, $3, 'info', 'item', $4, now() - interval '1 hour')
		RETURNING id`, testWorkspaceID, testUserID, itemType, issueID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// handOff moves an issue assigned to the owner in todo to an agent in
// in_progress by a workflow handoff, publishing the payload the issue handler
// sends.
func handOff(t *testing.T, bus *events.Bus, issueID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status = 'in_progress', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`,
		issueID, handoffAgentID); err != nil {
		t.Fatal(err)
	}
	agentType, memberType, owner, agentID := "agent", "member", testUserID, handoffAgentID
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
}
