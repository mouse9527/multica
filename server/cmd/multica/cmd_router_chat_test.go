// Added by the mouse9527/multica fork: Router settings by instruction (M-105).

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const (
	testChatID    = "0a1b2c3d-0000-4000-8000-000000000001"
	testIssueID   = "0a1b2c3d-0000-4000-8000-000000000002"
	testProjectID = "0a1b2c3d-0000-4000-8000-000000000003"
	testSubA      = "0a1b2c3d-0000-4000-8000-00000000000a"
	testSubB      = "0a1b2c3d-0000-4000-8000-00000000000b"
)

type routerCall struct {
	Method, Path string
	Body         map[string]any
}

// routerServer fakes the Router API for one Router (testSubA, testSubB bound
// to testChatID) and records every request. failSubscribe makes subscription
// creation fail.
func routerServer(t *testing.T, failSubscribe bool) *[]routerCall {
	t.Helper()
	calls := &[]routerCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := routerCall{Method: r.Method, Path: r.URL.RequestURI()}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &call.Body)
		}
		*calls = append(*calls, call)
		sub := func(id string, enabled bool) map[string]any {
			return map[string]any{"id": id, "agent_id": "agent-1", "scope_type": "issue_tree", "scope_id": testIssueID, "enabled": enabled}
		}
		switch {
		case r.URL.Path == "/api/router/chats/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"chat_session_id": testChatID, "title": "Feature lead"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/router/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"subscriptions": []any{sub(testSubA, true), sub(testSubB, false)}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{map[string]any{"id": testProjectID, "title": "Platform"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "board-1", "identifier": "M-9"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/router/subscriptions":
			if failSubscribe {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "an agent can bind a chat to a Router only in a run started by that chat's creator"})
				return
			}
			_ = json.NewEncoder(w).Encode(sub("new", true))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/api/router/subscriptions/"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/router/subscriptions/"), "/")[0]
			_ = json.NewEncoder(w).Encode(sub(id, id == testSubA))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setCopyTestEnv(t, srv.URL)
	return calls
}

// withFlags sets flags on a shared command and restores their defaults after
// the test.
func withFlags(t *testing.T, cmd *cobra.Command, flags map[string]string) {
	t.Helper()
	for name, value := range flags {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("--%s is not a flag of %s", name, cmd.Name())
		}
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
}

func writes(calls []routerCall) []string {
	var out []string
	for _, c := range calls {
		if c.Method != http.MethodGet {
			out = append(out, c.Method+" "+c.Path)
		}
	}
	return out
}

func TestRouterSubscribeToCurrentChat(t *testing.T) {
	calls := routerServer(t, false)
	withFlags(t, routerSubscribeCmd, map[string]string{"issue": testIssueID, "anchor-chat": "current"})
	if err := runRouterSubscribe(routerSubscribeCmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := writes(*calls); len(got) != 1 || got[0] != "POST /api/router/subscriptions" {
		t.Fatalf("writes = %v", got)
	}
	body := (*calls)[len(*calls)-1].Body
	if body["anchor_chat_session_id"] != testChatID || body["scope_id"] != testIssueID || body["anchor_issue_id"] != nil {
		t.Fatalf("subscribe body = %v", body)
	}
}

func TestRouterSubscribeProjectToChatMakesABoard(t *testing.T) {
	calls := routerServer(t, false)
	withFlags(t, routerSubscribeCmd, map[string]string{"project": testProjectID, "anchor-chat": "current"})
	if err := runRouterSubscribe(routerSubscribeCmd, nil); err != nil {
		t.Fatal(err)
	}
	got := writes(*calls)
	if strings.Join(got, ", ") != "POST /api/issues, POST /api/router/subscriptions" {
		t.Fatalf("writes = %v", got)
	}
	for _, c := range *calls {
		if c.Path == "/api/issues" && (c.Body["project_id"] != testProjectID || c.Body["title"] != "Super Agent 看板 · Feature lead") {
			t.Fatalf("board issue = %v", c.Body)
		}
		if c.Method == http.MethodPost && c.Path == "/api/router/subscriptions" && c.Body["anchor_issue_id"] != "board-1" {
			t.Fatalf("subscription anchor = %v", c.Body)
		}
	}
}

func TestRouterSubscribeRefusedDeletesTheBoard(t *testing.T) {
	calls := routerServer(t, true)
	withFlags(t, routerSubscribeCmd, map[string]string{"project": testProjectID, "anchor-chat": testChatID})
	err := runRouterSubscribe(routerSubscribeCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "chat's creator") {
		t.Fatalf("err = %v, want the server's refusal", err)
	}
	if got := writes(*calls); strings.Join(got, ", ") != "POST /api/issues, POST /api/router/subscriptions, DELETE /api/issues/board-1" {
		t.Fatalf("writes = %v", got)
	}
}

func TestRouterPauseCurrentChatPausesEverySubscription(t *testing.T) {
	calls := routerServer(t, false)
	withFlags(t, routerPauseCmd, map[string]string{"chat": "current"})
	if err := runRouterAction(routerPauseCmd, nil, "pause"); err != nil {
		t.Fatal(err)
	}
	want := "POST /api/router/subscriptions/" + testSubA + "/pause, POST /api/router/subscriptions/" + testSubB + "/pause"
	if got := strings.Join(writes(*calls), ", "); got != want {
		t.Fatalf("writes = %s", got)
	}
	if (*calls)[1].Path != "/api/router/subscriptions?chat_session_id="+testChatID {
		t.Fatalf("the Router was looked up as %s", (*calls)[1].Path)
	}
}

func TestRouterReleaseKeepPausesBeforeUnbinding(t *testing.T) {
	calls := routerServer(t, false)
	withFlags(t, routerReleaseCmd, map[string]string{"chat": testChatID, "keep": "true"})
	if err := runRouterRelease(routerReleaseCmd, nil); err != nil {
		t.Fatal(err)
	}
	a, b := "/api/router/subscriptions/"+testSubA, "/api/router/subscriptions/"+testSubB
	// testSubB is already paused, so it is only unbound.
	want := "POST " + a + "/pause, PATCH " + a + ", PATCH " + b
	if got := strings.Join(writes(*calls), ", "); got != want {
		t.Fatalf("writes = %s", got)
	}
	for _, c := range *calls {
		if c.Method == http.MethodPatch && c.Body["anchor_chat_session_id"] != "none" {
			t.Fatalf("unbind body = %v", c.Body)
		}
	}
}

func TestRouterReleaseDeletes(t *testing.T) {
	calls := routerServer(t, false)
	withFlags(t, routerReleaseCmd, map[string]string{"chat": "current"})
	if err := runRouterRelease(routerReleaseCmd, nil); err != nil {
		t.Fatal(err)
	}
	want := "DELETE /api/router/subscriptions/" + testSubA + ", DELETE /api/router/subscriptions/" + testSubB
	if got := strings.Join(writes(*calls), ", "); got != want {
		t.Fatalf("writes = %s", got)
	}
}

func TestRouterUpdateBindsTheCurrentChat(t *testing.T) {
	calls := routerServer(t, false)
	withFlags(t, routerUpdateCmd, map[string]string{"anchor-chat": "current"})
	if err := runRouterUpdate(routerUpdateCmd, []string{testSubA}); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if last.Method != http.MethodPatch || last.Body["anchor_chat_session_id"] != testChatID {
		t.Fatalf("update = %+v", last)
	}
}

func TestRouterTargetsNeedAnIDOrAChat(t *testing.T) {
	routerServer(t, false)
	if err := runRouterAction(routerResumeCmd, nil, "resume"); err == nil {
		t.Fatal("resume without an id or --chat succeeded")
	}
	withFlags(t, routerResumeCmd, map[string]string{"chat": "current"})
	if err := runRouterAction(routerResumeCmd, []string{testSubA}, "resume"); err == nil {
		t.Fatal("resume with both an id and --chat succeeded")
	}
	if err := runRouterRelease(routerReleaseCmd, nil); err == nil {
		t.Fatal("release without --chat succeeded")
	}
}
