// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// TestRouterDispatchesRealAnchorRun wires capture and dispatch the way
// main.go does and goes end to end through the real task service: a member's
// comment in the subscribed tree becomes, after the debounce, one queued run
// of the subscribed agent on the anchor issue whose handoff note carries the
// batch; the state and notice APIs work on the same subscription.
func TestRouterDispatchesRealAnchorRun(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.RouterV1, featureflag.Rule{Default: true})
	provider.Set(featureflags.RouterDispatch, featureflag.Rule{Default: true})
	flags := featureflag.NewService(provider)

	bus := events.New()
	hub := realtime.NewHub()
	go hub.Run()
	router, h := NewRouterWithOptions(testPool, hub, bus, analytics.NoopClient{}, nil, RouterOptions{FeatureFlags: flags})
	jobs := &routerJobs{}
	if !agentrouter.Register(ctx, flags, bus, testPool, jobs, h.TaskService) || len(jobs.specs) != 2 ||
		jobs.specs[1].Name != agentrouter.JobNameDispatch {
		t.Fatalf("router not registered with dispatch: jobs=%d", len(jobs.specs))
	}
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

	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agent := fx.Agent(t, "router dispatch e2e agent", fx.Runtime(t, "router dispatch e2e runtime"))

	var root map[string]any
	call("POST", "/api/issues", map[string]any{"title": "router dispatch e2e root"}, http.StatusCreated, &root)
	rootID := root["id"].(string)
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM agent_task_queue WHERE agent_id = $1`,
			`DELETE FROM router_event WHERE agent_id = $1`,
			`DELETE FROM router_dispatch WHERE agent_id = $1`,
			`DELETE FROM router_issue_seen WHERE subscription_id IN (SELECT id FROM router_subscription WHERE agent_id = $1)`,
			`DELETE FROM owner_notice WHERE source_subscription_id IN (SELECT id FROM router_subscription WHERE agent_id = $1)`,
			`DELETE FROM router_subscription_change WHERE subscription_id IN (SELECT id FROM router_subscription WHERE agent_id = $1)`,
			`DELETE FROM router_subscription WHERE agent_id = $1`,
		} {
			_, _ = testPool.Exec(bg, q, agent)
		}
		_, _ = testPool.Exec(bg, `DELETE FROM inbox_item WHERE workspace_id = $1 AND type = $2`, testWorkspaceID, agentrouter.InboxNoticeType)
		_, _ = testPool.Exec(bg, `DELETE FROM issue WHERE id = $1 OR parent_issue_id = $1`, rootID)
	})

	var sub map[string]any
	call("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": agent, "scope_type": "issue_tree", "scope_id": rootID, "debounce_seconds": 10,
	}, http.StatusCreated, &sub)
	subID := sub["id"].(string)
	if sub["anchor_issue_id"] != rootID || sub["debounce_seconds"] != float64(10) || sub["max_runs_per_hour"] != float64(6) {
		t.Fatalf("subscription = %v", sub)
	}

	call("POST", "/api/issues/"+rootID+"/comments", map[string]any{"content": "router dispatch e2e comment"}, http.StatusCreated, nil)
	if _, err := testPool.Exec(ctx, `UPDATE router_event SET created_at = created_at - interval '1 hour' WHERE subscription_id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	d := agentrouter.NewDispatcher(agentrouter.NewStore(testPool, testPool), agentrouter.TaskRuns{Tasks: h.TaskService},
		agentrouter.NewNotifier(agentrouter.NewStore(testPool, testPool), agentrouter.InboxAdapter{Queries: db.New(testPool), Bus: bus}))
	if _, err := d.TickWorkspace(ctx, testWorkspaceID); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var taskID, status, note string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text, status, COALESCE(handoff_note, '') FROM agent_task_queue
		WHERE agent_id = $1 AND issue_id = $2`, agent, rootID).Scan(&taskID, &status, &note); err != nil {
		t.Fatalf("anchor run: %v", err)
	}
	if status != "queued" || !strings.Contains(note, "Router batch for subscription "+subID) || !strings.Contains(note, "comment_created") {
		t.Fatalf("anchor run status %s, note:\n%s", status, note)
	}
	var runs struct {
		Dispatches []map[string]any `json:"dispatches"`
	}
	call("GET", "/api/router/dispatches?subscription_id="+subID, nil, http.StatusOK, &runs)
	if len(runs.Dispatches) != 1 || runs.Dispatches[0]["task_id"] != taskID || runs.Dispatches[0]["event_count"] != float64(1) {
		t.Fatalf("dispatches = %v", runs.Dispatches)
	}

	var st map[string]any
	call("PUT", "/api/router/subscriptions/"+subID+"/state", map[string]any{"state": "seen RT", "expected_revision": 0}, http.StatusOK, &st)
	if st["revision"] != float64(1) {
		t.Fatalf("state = %v", st)
	}
	call("PUT", "/api/router/subscriptions/"+subID+"/state", map[string]any{"state": "stale", "expected_revision": 0}, http.StatusConflict, nil)

	var sent struct {
		Notice  map[string]any `json:"notice"`
		Created bool           `json:"created"`
	}
	call("POST", "/api/router/notices", map[string]any{
		"subscription_id": subID, "title": "router e2e notice", "refs": []string{rootID}, "dedupe_key": "router-e2e-" + subID,
	}, http.StatusCreated, &sent)
	inbox, _ := sent.Notice["channel_states"].(map[string]any)["inbox"].(map[string]any)
	if !sent.Created || sent.Notice["recipient_user_id"] != testUserID || inbox["state"] != agentrouter.DeliverySent {
		t.Fatalf("notice = %v", sent)
	}
}
