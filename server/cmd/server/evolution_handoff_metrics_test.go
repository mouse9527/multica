// Added by the mouse9527/multica fork: Evolution structural analysis, the
// metrics source (M-296).

package main

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/handler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestEvolutionMetricsCountsWorkflowHandoffs runs the activity rows the real
// listener writes through evolution.ReadHandoffs: an assignment, then a
// workflow step entry that hands the issue on. The listener folds the second
// handoff into the status row (no assignee row), so the metrics read it from
// handoff_to_*, once, and take who it came from from the row itself when the
// Router annotates it, or else from the previous handoff.
func TestEvolutionMetricsCountsWorkflowHandoffs(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	for _, annotate := range []bool{false, true} {
		name := "annotation off"
		if annotate {
			name = "annotation on"
		}
		t.Run(name, func(t *testing.T) {
			agentrouter.SetActivityAnnotation(annotate)
			t.Cleanup(func() { agentrouter.SetActivityAnnotation(false) })

			queries := db.New(testPool)
			bus := events.New()
			registerActivityListeners(bus, queries)
			issueID := createTestIssue(t, testWorkspaceID, testUserID)
			t.Cleanup(func() {
				cleanupActivities(t, issueID)
				cleanupTestIssue(t, issueID)
			})

			agentType := "agent"
			developer := "0f0e0d0c-0b0a-4909-8807-0605040302a1"
			reviewer := "0f0e0d0c-0b0a-4909-8807-0605040302b2"
			issue := func(status, assignee string) handler.IssueResponse {
				return handler.IssueResponse{
					ID: issueID, WorkspaceID: testWorkspaceID, Title: "evolution handoff metrics", Status: status,
					Priority: "medium", CreatorType: "member", CreatorID: testUserID,
					AssigneeType: &agentType, AssigneeID: &assignee,
				}
			}
			publish := func(payload map[string]any) {
				bus.Publish(events.Event{
					Type: protocol.EventIssueUpdated, WorkspaceID: testWorkspaceID,
					ActorType: "member", ActorID: testUserID, Payload: payload,
				})
			}
			// Assigned to the developer by hand.
			publish(map[string]any{
				"issue":              issue("todo", developer),
				"assignee_changed":   true,
				"prev_assignee_type": (*string)(nil),
				"prev_assignee_id":   (*string)(nil),
			})
			// Entering review hands it to the reviewer.
			publish(map[string]any{
				"issue":              issue("in_review", reviewer),
				"status_changed":     true,
				"prev_status":        "todo",
				"assignee_changed":   true,
				"prev_assignee_type": &agentType,
				"prev_assignee_id":   &developer,
				"workflow_handoff": map[string]string{
					"workflow": "Delivery", "to_type": "agent", "to_id": reviewer, "run": "true",
				},
			})

			now := time.Now()
			all, err := evolution.ReadHandoffs(context.Background(), testPool, testWorkspaceID,
				now.Add(-time.Hour), now.Add(time.Hour), nil, nil)
			if err != nil {
				t.Fatalf("ReadHandoffs: %v", err)
			}
			var got []evolution.MetricsHandoff
			for _, h := range all {
				if h.IssueID == issueID {
					got = append(got, h)
				}
			}
			if len(got) != 2 {
				t.Fatalf("handoffs = %+v, want the assignment and the workflow handoff once each", got)
			}
			if got[0].ToType != "agent" || got[0].ToID != developer || got[0].FromID != "" {
				t.Fatalf("assignment = %+v", got[0])
			}
			if got[1].FromType != "agent" || got[1].FromID != developer || got[1].ToType != "agent" || got[1].ToID != reviewer {
				t.Fatalf("workflow handoff = %+v", got[1])
			}

			summary := evolution.SummarizeAgents([]evolution.MetricsAgent{{ID: developer}, {ID: reviewer}}, nil, got)
			if summary[0].HandoffsIn != 1 || summary[0].HandoffsOut != 1 || summary[1].HandoffsIn != 1 || summary[1].HandoffsOut != 0 {
				t.Fatalf("summary = %+v", summary)
			}
		})
	}
}
