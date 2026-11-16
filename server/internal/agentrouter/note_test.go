// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).

package agentrouter

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildNote(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	member, agent := "member", "agent"
	var evs []batchEvent
	for i := range 200 {
		evs = append(evs, batchEvent{
			ID: fmt.Sprint(i), Kind: KindCommentCreated, IssueKey: "RT-2", IssueTitle: strings.Repeat("long title ", 20),
			ActorType: &member, ActorName: "Mouse", Payload: map[string]any{"comment_id": "c" + fmt.Sprint(i)},
			OccurredAt: now.Add(-time.Hour),
		})
	}
	evs[0] = batchEvent{ID: "s", Kind: KindCommentCreated, IssueKey: "RT-1", ActorType: &agent, ActorName: "Router", Self: true,
		Payload: map[string]any{"comment_id": "own"}, OccurredAt: now}
	evs[1] = batchEvent{ID: "st", Kind: KindStatusChanged, IssueKey: "RT-3", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"from": "todo", "to": "in_review"}, OccurredAt: now}
	evs[2] = batchEvent{ID: "f", Kind: KindTaskFailed, IssueKey: "RT-4", Payload: map[string]any{"failure_reason": "timeout"}, OccurredAt: now}

	note := buildNote(noteInput{
		sub:   dispatchSub{ID: "sub-1", StateRevision: 3, State: ""},
		batch: batch{events: evs}, anchorKey: "RT-1", scopeLabel: "issue tree RT-1",
		inflight:      []inFlightIssue{{Key: "RT-3", Title: "review me", Status: "in_review", AssigneeType: &agent, AssigneeName: "Reviewer", UpdatedAt: now.Add(-3 * time.Hour)}},
		inflightTotal: 41, now: now,
	})
	for _, want := range []string{
		"Router batch for subscription sub-1 (issue tree RT-1, anchor RT-1): 199 new event(s).",
		"RT-3 status_changed todo -> in_review by member Mouse",
		"RT-4 task_failed (timeout)",
		"more: `multica router events --subscription sub-1 --state pending --output json`",
		"- RT-3 [in_review] agent Reviewer, updated 3h ago: review me",
		"(1 of 41,",
		"the rest: `multica router scope sub-1`",
		"State (revision 3)",
		"--expect-revision 3 --state-file",
		"(empty)",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q", want)
		}
	}
	if strings.Contains(note, "comment own") || strings.Contains(note, "by agent Router") {
		t.Error("note lists the agent's own event")
	}
	if len(note) > 9*1024 {
		t.Errorf("note is %d bytes; the event list should be capped", len(note))
	}
}
