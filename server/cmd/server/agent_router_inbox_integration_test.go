// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
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
			agentrouter.SetActivityAnnotation(true)
			t.Cleanup(func() { agentrouter.SetActivityAnnotation(false) })
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

// TestInboxResolveThroughBatchUpdate drives the real batch update endpoint:
// a workflow handoff and a plain reassignment away from the owner of an issue
// the owner created and assigned to themselves (only an ordinary comment
// item, no issue_assigned). The inbox listener archives the item; with the
// listener failing, reconcile archives it from the activity rows the upstream
// activity listener wrote.
func TestInboxResolveThroughBatchUpdate(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	flags := featureflag.NewService(provider)
	bus := events.New()
	hub := realtime.NewHub()
	go hub.Run()
	router, _ := NewRouterWithOptions(testPool, hub, bus, analytics.NoopClient{}, nil, RouterOptions{FeatureFlags: flags})
	srv := httptest.NewServer(router)
	defer srv.Close()
	call := func(method, path string, body any, want int, out any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			var msg map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&msg)
			t.Fatalf("%s %s = %d (%v), want %d", method, path, resp.StatusCode, msg, want)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("decode %s %s: %v", method, path, err)
			}
		}
	}

	registerActivityListeners(bus, db.New(testPool))
	agentrouter.SetActivityAnnotation(true)
	t.Cleanup(func() { agentrouter.SetActivityAnnotation(false) })
	live := agentrouter.NewInboxResolver(testPool, bus)
	live.Subscribe(bus)
	cfg := agentrouter.InboxResolveConfig{OwnerUserID: testUserID, WaitingStatuses: []string{"started_2"}}
	if err := agentrouter.SetInboxResolveConfig(ctx, testPool, testWorkspaceID, &cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - $2 WHERE id = $1`,
			testWorkspaceID, agentrouter.WorkspaceSettingInboxResolve)
	})

	// A project whose workflow hands in_progress to an agent.
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agent := fx.Agent(t, "inbox resolve batch agent", fx.Runtime(t, "inbox resolve batch runtime"))
	var workflow map[string]any
	call("POST", "/api/issue-workflows", map[string]any{
		"name": fmt.Sprintf("Inbox resolve %d", time.Now().UnixNano()),
		"steps": []map[string]any{{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "in_progress"},
			{"status_key": "done"}, {"status_key": "blocked"}},
	}, http.StatusCreated, &workflow)
	workflowID := workflow["id"].(string)
	project := fx.Project(t, "inbox resolve batch project")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE project_id = $1`, project)
		_, _ = testPool.Exec(ctx, `UPDATE project SET workflow_id = NULL WHERE id = $1`, project)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	})
	call("POST", "/api/projects/"+project+"/workflow", map[string]any{"workflow_id": workflowID}, http.StatusOK, nil)
	call("PUT", "/api/issue-workflows/"+workflowID+"/handoffs/in_progress", map[string]any{
		"handler": map[string]any{"type": "agent", "id": agent}, "instructions": "Take it from here.",
	}, http.StatusOK, nil)

	type scenario struct {
		updates   map[string]any
		inProject bool
		failing   bool
	}
	handoff := map[string]any{"status": "in_progress"}
	reassign := map[string]any{"assignee_type": "agent", "assignee_id": agent}
	for name, sc := range map[string]scenario{
		"handoff, listener":              {updates: handoff, inProject: true},
		"handoff, listener failing":      {updates: handoff, inProject: true, failing: true},
		"reassignment, listener":         {updates: reassign},
		"reassignment, listener failing": {updates: reassign, failing: true},
	} {
		t.Run(name, func(t *testing.T) {
			live.Timeout = 5 * time.Second
			if sc.failing {
				live.Timeout = time.Nanosecond
			}
			cols := testutil.Cols{"status": "todo", "assignee_type": "member", "assignee_id": testUserID}
			if sc.inProject {
				cols["project_id"] = project
			}
			issueID := fx.Issue(t, "inbox resolve batch "+name, cols)
			t.Cleanup(func() {
				cleanupActivities(t, issueID)
				_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
				_, _ = testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
			})
			itemID := ownerItem(t, issueID, "new_comment")

			call("POST", "/api/issues/batch-update", map[string]any{"issue_ids": []string{issueID}, "updates": sc.updates},
				http.StatusOK, nil)
			var assigneeID string
			if err := testPool.QueryRow(ctx, `SELECT assignee_id::text FROM issue WHERE id = $1`, issueID).Scan(&assigneeID); err != nil || assigneeID != agent {
				t.Fatalf("assignee after the batch update = %q (%v), want the agent", assigneeID, err)
			}
			archived := func() bool {
				var a bool
				if err := testPool.QueryRow(ctx, `SELECT archived FROM inbox_item WHERE id = $1`, itemID).Scan(&a); err != nil {
					t.Fatal(err)
				}
				return a
			}
			if got := archived(); got == sc.failing {
				t.Fatalf("archived right after the batch update = %v, want %v", got, !sc.failing)
			}
			if !sc.failing {
				return
			}
			r := agentrouter.NewInboxResolver(testPool, nil)
			r.Grace = 0
			if _, err := r.ReconcileWorkspace(ctx, testWorkspaceID); err != nil {
				t.Fatal(err)
			}
			if !archived() {
				t.Fatal("reconcile left the owner's item after a batch update the listener missed")
			}
		})
	}

	// With inbox resolve off, the batch payload and activity rows are
	// upstream's.
	t.Run("annotation off", func(t *testing.T) {
		agentrouter.SetActivityAnnotation(false)
		live.Timeout = time.Nanosecond
		var payload map[string]any
		bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
			if p, ok := e.Payload.(map[string]any); ok {
				payload = p
			}
		})
		issueID := fx.Issue(t, "inbox resolve batch off", testutil.Cols{"status": "todo", "assignee_type": "member", "assignee_id": testUserID})
		t.Cleanup(func() { cleanupActivities(t, issueID) })
		call("POST", "/api/issues/batch-update", map[string]any{"issue_ids": []string{issueID}, "updates": reassign}, http.StatusOK, nil)
		if _, has := payload[agentrouter.PayloadPrevAssigneeType]; has {
			t.Fatalf("batch payload carries %s with inbox resolve off", agentrouter.PayloadPrevAssigneeType)
		}
		var annotated bool
		if err := testPool.QueryRow(ctx, `SELECT details ? 'router_from_type' FROM activity_log WHERE issue_id = $1 AND action = 'assignee_changed'`,
			issueID).Scan(&annotated); err != nil || annotated {
			t.Fatalf("assignee row has router_from_type = %v (%v), want upstream's row", annotated, err)
		}
	})
}
