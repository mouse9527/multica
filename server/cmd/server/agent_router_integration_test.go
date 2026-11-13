// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

type routerJobs struct{ specs []scheduler.JobSpec }

func (r *routerJobs) Register(spec scheduler.JobSpec) error {
	r.specs = append(r.specs, spec)
	return nil
}

// TestRouterCapturesRealWrites wires the Router the way main.go does and
// drives it through the real HTTP write paths: a status change, a comment and
// a new child issue are captured by the listener; a change written behind the
// server's back is added by reconcile and counted as capture_miss.
func TestRouterCapturesRealWrites(t *testing.T) {
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
	jobs := &routerJobs{}
	if !agentrouter.Register(ctx, flags, bus, testPool, jobs) || len(jobs.specs) != 1 {
		t.Fatalf("router not registered: jobs=%d", len(jobs.specs))
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
	agent := fx.Agent(t, "router e2e agent", fx.Runtime(t, "router e2e runtime"))

	var root map[string]any
	call("POST", "/api/issues", map[string]any{"title": "router e2e root"}, http.StatusCreated, &root)
	rootID := root["id"].(string)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM router_event WHERE agent_id = $1`, agent)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM router_issue_seen WHERE subscription_id IN (SELECT id FROM router_subscription WHERE agent_id = $1)`, agent)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM router_subscription WHERE agent_id = $1`, agent)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1 OR parent_issue_id = $1`, rootID)
	})

	var sub map[string]any
	call("POST", "/api/router/subscriptions", map[string]any{
		"agent_id": agent, "scope_type": "issue_tree", "scope_id": rootID,
	}, http.StatusCreated, &sub)
	subID := sub["id"].(string)

	call("PUT", "/api/issues/"+rootID, map[string]any{"status": "in_progress"}, http.StatusOK, nil)
	call("POST", "/api/issues/"+rootID+"/comments", map[string]any{"content": "router e2e comment"}, http.StatusCreated, nil)
	call("POST", "/api/issues", map[string]any{"title": "router e2e child", "parent_issue_id": rootID}, http.StatusCreated, nil)

	listKinds := func() []string {
		var out struct {
			Events []map[string]any `json:"events"`
		}
		call("GET", "/api/router/events?subscription_id="+subID, nil, http.StatusOK, &out)
		var kinds []string
		for _, e := range out.Events {
			kinds = append(kinds, e["kind"].(string)+"/"+e["source"].(string))
		}
		sort.Strings(kinds)
		return kinds
	}
	want := []string{"comment_created/bus", "issue_created/bus", "status_changed/bus"}
	if got := listKinds(); !equalStrings(got, want) {
		t.Fatalf("captured = %v, want %v", got, want)
	}

	// A write that never publishes: the listener cannot see it.
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'in_review', updated_at = now() WHERE id = $1`, rootID); err != nil {
		t.Fatal(err)
	}
	recon := agentrouter.NewReconciler(agentrouter.NewStore(testPool, testPool))
	recon.Grace = 0
	res, err := recon.RunWorkspace(ctx, testWorkspaceID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Missed[agentrouter.KindStatusChanged] < 1 {
		t.Fatalf("reconcile missed = %v, want the status change", res.Missed)
	}
	call("GET", "/api/router/subscriptions/"+subID, nil, http.StatusOK, &sub)
	if sub["capture_miss"] != float64(1) {
		t.Fatalf("capture_miss = %v, want 1", sub["capture_miss"])
	}
	want = append(want, "status_changed/reconcile")
	sort.Strings(want)
	if got := listKinds(); !equalStrings(got, want) {
		t.Fatalf("after reconcile = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
