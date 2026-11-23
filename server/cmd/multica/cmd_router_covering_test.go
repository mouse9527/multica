// Added by the mouse9527/multica fork: which Super Agents cover an issue, the
// scope hint on `issue create`, and the overlap refusal on subscribe (M-194).

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestRouterCoveringIsRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"router", "covering"})
	if err != nil || cmd.Name() != "covering" {
		t.Fatalf("multica router covering: %v", err)
	}
	if routerSubscribeCmd.Flags().Lookup("allow-overlap") == nil {
		t.Fatal("--allow-overlap missing on router subscribe")
	}
}

func TestRouterCoveringRows(t *testing.T) {
	rows := routerCoveringRows([]map[string]any{
		{"subscription_id": "sub-tree", "agent_name": "Mika", "scope_type": "issue_tree", "scope_name": "M-45",
			"chat_bound": true, "chat_session_id": "chat-1", "chat_title": "triage", "enabled": true, "effective": true},
		{"subscription_id": "sub-proj", "agent_name": "Mika", "scope_type": "project", "scope_name": "multica",
			"chat_bound": true, "enabled": true, "effective": false, "superseded_by": "sub-tree"},
		{"subscription_id": "sub-ops", "agent_id": "agent-2", "scope_type": "project", "scope_id": "proj-2",
			"chat_bound": false, "enabled": false, "effective": false},
	}, true)
	want := [][]string{
		{"sub-tree", "Mika", "issue_tree:M-45", "triage chat-1", "running", "yes"},
		{"sub-proj", "Mika", "project:multica", "(another member's chat)", "running", "no (narrower sub-tree)"},
		{"sub-ops", "agent-2", "project:proj-2", "-", "paused", "no"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("rows =\n%v\nwant\n%v", rows, want)
	}
}

func TestSuperAgentScopeHintOnIssueCreate(t *testing.T) {
	for name, tc := range map[string]struct {
		issue map[string]any
		want  string
	}{
		"not a Super Agent chat": {map[string]any{"id": "i"}, ""},
		"in scope":               {map[string]any{"super_agent_scope": map[string]any{"in_scope": true}}, ""},
		"out of scope": {map[string]any{"super_agent_scope": map[string]any{"in_scope": false, "hint": "M-9 is not in this Super Agent's scope"}},
			"Super Agent: M-9 is not in this Super Agent's scope\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			printSuperAgentScopeHint(&out, tc.issue)
			if out.String() != tc.want {
				t.Fatalf("stderr = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestRouterOverlapMessage(t *testing.T) {
	err := fmt.Errorf("post: %w", &cli.HTTPError{StatusCode: http.StatusConflict, Body: `{"code":"router_scope_overlap","error":"x",
		"overlaps":[{"subscription_id":"sub-1","agent_name":"ops","scope_type":"project","scope_name":"home","issue_count":12,"issues":["M-1","M-2"]}]}`})
	msg, ok := routerOverlapMessage(err)
	if !ok {
		t.Fatal("overlap refusal not recognised")
	}
	for _, want := range []string{"ops, subscription sub-1 (project home): 12 issue(s), e.g. M-1, M-2", "--allow-overlap", "multica router covering"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if _, ok := routerOverlapMessage(&cli.HTTPError{StatusCode: http.StatusConflict, Body: `{"error":"subscription already exists"}`}); ok {
		t.Fatal("a duplicate subscription read as an overlap")
	}
	if _, ok := routerOverlapMessage(errors.New("boom")); ok {
		t.Fatal("a plain error read as an overlap")
	}
}
