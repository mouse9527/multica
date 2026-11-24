// Added by the mouse9527/multica fork: the Evolution agent's server-side
// writes are marked on their events (M-233).

package handler

import (
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestEvolutionServerWritesAreMarked: the analysis issue the scheduler
// creates, the proposal issue and the evidence comment the proposal store
// adds are all published as server writes in the Evolution agent's name,
// with no run named, so a Super Agent of the Evolution agent hears them.
func TestEvolutionServerWritesAreMarked(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var mu sync.Mutex
	var got []events.Event
	// The bus is shared with other tests: keep only marked events.
	keep := func(e events.Event) {
		if e.ServerWrite {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		}
	}
	testHandler.Bus.Subscribe(protocol.EventIssueCreated, keep)
	testHandler.Bus.Subscribe(protocol.EventCommentCreated, keep)
	marked := func(eventType, actorID string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, e := range got {
			if e.Type == eventType && e.ActorType == "agent" && e.ActorID == actorID && e.ActorTaskID == "" {
				n++
			}
		}
		return n
	}

	f := newProposalFixture(t, evolution.DefaultConfig())
	if n := marked(protocol.EventIssueCreated, f.evo); n != 1 {
		t.Fatalf("analysis issue: %d marked created events, want 1", n)
	}

	var first evolution.FileResult
	propose(t, f.evo, f.taskID, f.proposal()).Want(http.StatusCreated).JSON(&first)
	if n := marked(protocol.EventIssueCreated, f.evo); n != 2 {
		t.Fatalf("after the proposal: %d marked created events, want 2", n)
	}

	more := f.proposal()
	issueC := dbfx.Issue(t, "evolution server write evidence C")
	more["evidence"] = append(more["evidence"].([]map[string]any), map[string]any{"type": "issue", "id": issueC, "note": "a third case"})
	propose(t, f.evo, f.taskID, more).Want(http.StatusOK)
	if n := marked(protocol.EventCommentCreated, f.evo); n != 1 {
		t.Fatalf("new evidence: %d marked comment events, want 1", n)
	}
}
