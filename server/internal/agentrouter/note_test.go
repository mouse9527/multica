// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), the Router's boundary (M-112), self-contained owner decisions (M-162), and the chat wakeup and batch split (M-164).

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
		"Super Agent batch for subscription sub-1 (issue tree RT-1, anchor RT-1): 199 new event(s).",
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

// Both carriers tell the Router to hand substantive work to workers and say
// where small and repository-worthy work goes (M-112).
func TestBuildNoteStatesRouterBoundary(t *testing.T) {
	member := "member"
	evs := []batchEvent{{ID: "1", Kind: KindCommentCreated, IssueKey: "RT-2", ActorType: &member, ActorName: "Mouse",
		Payload: map[string]any{"comment_id": "c1"}, OccurredAt: time.Now()}}
	for _, chat := range []bool{false, true} {
		note := buildNote(noteInput{sub: dispatchSub{ID: "sub-1"}, batch: batch{events: evs}, anchorKey: "RT-1",
			scopeLabel: "issue tree RT-1", now: time.Now(), chat: chat})
		for _, want := range []string{
			"you do not do the work yourself",
			"querying or analysing business data",
			"a small task becomes a local Multica issue assigned to a fitting worker",
			"also gets a GitHub issue, linked as `gh_issue` per the github-evidence skill",
			"ops, environment and credential work stays a local issue only",
			"`multica router guide`", // M-150: the full guide ships in the CLI
			"Every decision you ask of the owner must stand on its own", // M-162
			// M-164: batches, not chat wakeups, report issues in scope.
			"do not create a chat wakeup (`--until-*` condition or `--every` check) for an issue in scope",
			"A chat wakeup is still fine for issues outside the scope",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("chat=%v: note lacks %q", chat, want)
			}
		}
		if got := strings.Contains(note, "When the owner asks you something here"); got != chat {
			t.Errorf("chat=%v: owner-question rule present=%v", chat, got)
		}
	}
}
