// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package agentrouter

import (
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestPickNarrowest(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	// chain: child -> parent -> root; the child is in project P.
	chain := []string{"child", "parent", "root"}
	tree := func(id, agent, scope string, created time.Time) scopeSub {
		return scopeSub{ID: id, AgentID: agent, ScopeType: ScopeIssueTree, ScopeID: scope, CreatedAt: created}
	}
	proj := func(id, agent, scope string, created time.Time) scopeSub {
		return scopeSub{ID: id, AgentID: agent, ScopeType: ScopeProject, ScopeID: scope, CreatedAt: created}
	}

	cases := []struct {
		name string
		subs []scopeSub
		want []string
	}{
		{"no subscription matches", []scopeSub{tree("s1", "a", "other", t0), proj("s2", "a", "Q", t0)}, nil},
		{"issue tree beats project", []scopeSub{proj("s1", "a", "P", t0), tree("s2", "a", "root", t0.Add(time.Hour))}, []string{"s2"}},
		{"closer root beats further root", []scopeSub{tree("s1", "a", "root", t0), tree("s2", "a", "parent", t0.Add(time.Hour))}, []string{"s2"}},
		{"issue itself is the narrowest root", []scopeSub{tree("s1", "a", "parent", t0), tree("s2", "a", "child", t0.Add(time.Hour))}, []string{"s2"}},
		{"each agent gets its own narrowest", []scopeSub{
			proj("s1", "a", "P", t0), tree("s2", "a", "parent", t0),
			proj("s3", "b", "P", t0),
		}, []string{"s2", "s3"}},
		{"tie goes to the earliest subscription", []scopeSub{proj("s9", "a", "P", t0.Add(time.Hour)), proj("s1", "a", "P", t0)}, []string{"s1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickNarrowest(tc.subs, chain, "P")
			var ids []string
			for _, s := range got {
				ids = append(ids, s.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("pickNarrowest = %v, want %v", ids, tc.want)
			}
		})
	}

	if got := pickNarrowest([]scopeSub{proj("s1", "a", "P", t0)}, chain, ""); len(got) != 0 {
		t.Fatalf("issue without a project matched a project scope: %v", got)
	}
}

func TestNormalizeEventTypes(t *testing.T) {
	got, err := NormalizeEventTypes(nil)
	if err != nil || !slices.Equal(got, AllKinds) {
		t.Fatalf("empty = %v, %v; want all kinds", got, err)
	}
	got, err = NormalizeEventTypes([]string{"task_failed", " comment_created", "task_failed"})
	if err != nil || !slices.Equal(got, []string{KindCommentCreated, KindTaskFailed}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := NormalizeEventTypes([]string{"issue_deleted"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestReconcileIntervalFromEnv(t *testing.T) {
	if d, err := ReconcileIntervalFromEnv(""); err != nil || d != DefaultReconcileInterval {
		t.Fatalf("empty = %v, %v", d, err)
	}
	if d, err := ReconcileIntervalFromEnv("1m"); err != nil || d != time.Minute {
		t.Fatalf("1m = %v, %v", d, err)
	}
	for _, bad := range []string{"5", "10s", "soon"} {
		if _, err := ReconcileIntervalFromEnv(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestParseTrigger(t *testing.T) {
	actor := "11111111-1111-1111-1111-111111111111"
	type typedIssue struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	cases := []struct {
		name string
		e    events.Event
		want trigger
		ok   bool
	}{
		{
			name: "typed issue payload",
			e:    events.Event{Type: protocol.EventIssueUpdated, ActorType: "member", ActorID: actor, Payload: map[string]any{"issue": typedIssue{ID: "i1"}}},
			want: trigger{eventType: protocol.EventIssueUpdated, issueID: "i1", actorType: "member", actorID: actor},
			ok:   true,
		},
		{
			name: "map comment payload, system actor dropped",
			e:    events.Event{Type: protocol.EventCommentCreated, ActorType: "system", Payload: map[string]any{"comment": map[string]any{"id": "c1"}}},
			want: trigger{eventType: protocol.EventCommentCreated, commentID: "c1"},
			ok:   true,
		},
		{
			name: "terminal task failure",
			e:    events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"task_id": "t1", "issue_id": "i1", "retry_pending": false}},
			want: trigger{eventType: protocol.EventTaskFailed, taskID: "t1", issueID: "i1"},
			ok:   true,
		},
		{
			name: "failure with a retry queued is skipped",
			e:    events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"task_id": "t1", "issue_id": "i1", "retry_pending": true}},
		},
		{
			name: "chat task failure without an issue is skipped",
			e:    events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"task_id": "t1", "issue_id": ""}},
		},
		{
			name: "other event types are ignored",
			e:    events.Event{Type: protocol.EventIssueDeleted, Payload: map[string]any{"issue_id": "i1"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTrigger(tc.e)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseTrigger = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
