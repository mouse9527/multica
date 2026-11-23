// Modified for the mouse9527/multica fork: Router inbox S3b (M-133).

package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentrouter"
)

func TestRouterInboxRulesSettingAndReport(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newRouterFixture(t)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - $2 WHERE id = $1`,
			testWorkspaceID, agentrouter.WorkspaceSettingInboxRules)
	})

	withRouterFlags(t, false)
	routerCall(t, newRequest("GET", "/api/router/inbox-rules", nil)).Want(http.StatusNotFound)
	routerCall(t, newRequest("GET", "/api/router/inbox-rules/report", nil)).Want(http.StatusNotFound)
	withRouterFlags(t, true)

	type state struct {
		Enabled     bool                          `json:"enabled"`
		Config      *agentrouter.InboxRulesConfig `json:"config"`
		ConfigError string                        `json:"config_error"`
		Stats       *agentrouter.InboxRulesStats  `json:"stats"`
	}
	var before state
	routerCall(t, newRequest("GET", "/api/router/inbox-rules", nil)).Want(http.StatusOK).JSON(&before)
	if before.Config != nil || before.Enabled || before.Stats != nil {
		t.Fatalf("before = %+v, want no rules and classification off here", before)
	}

	// Only owners and admins, and only members, change it.
	defaults := map[string]any{"defaults": true, "waiting_statuses": []string{"in_review"}}
	routerCall(t, newRequestAs(f.member, "PUT", "/api/router/inbox-rules", defaults)).Want(http.StatusForbidden)
	routerCall(t, asAgent(newRequest("PUT", "/api/router/inbox-rules", defaults), f.agentA, f.taskA)).Want(http.StatusForbidden)
	for name, body := range map[string]map[string]any{
		"defaults without a status": {"defaults": true},
		"unknown status":            {"defaults": true, "waiting_statuses": []string{"no_such_status"}},
		"nothing":                   {},
		"rules and defaults":        {"defaults": true, "waiting_statuses": []string{"in_review"}, "rules": []map[string]any{{"name": "c", "types": []string{"new_comment"}, "verdict": "handle"}}},
		"a gray type handled":       {"rules": []map[string]any{{"name": "m", "types": []string{"mentioned"}, "verdict": "handle"}}},
		"a gray rule on comments":   {"rules": []map[string]any{{"name": "c", "types": []string{"new_comment"}, "verdict": "gray"}}},
		"unknown field":             {"rules": []map[string]any{}, "extra": 1},
	} {
		t.Run(name, func(t *testing.T) {
			routerCall(t, newRequest("PUT", "/api/router/inbox-rules", body)).Want(http.StatusBadRequest)
		})
	}

	var set state
	routerCall(t, newRequest("PUT", "/api/router/inbox-rules", defaults)).Want(http.StatusOK).JSON(&set)
	want := agentrouter.DefaultInboxRules([]string{"in_review"})
	if set.Config == nil || len(set.Config.Rules) != len(want.Rules) || set.Config.Rules[2].Name != "enter_waiting_status" ||
		len(set.Config.Rules[2].ToStatuses) != 1 || set.Config.Rules[2].ToStatuses[0] != "in_review" {
		t.Fatalf("after defaults = %+v", set.Config)
	}

	custom := map[string]any{"rules": []map[string]any{
		{"name": "comment", "types": []string{"new_comment"}, "verdict": "handle"},
		{"name": "mention", "types": []string{"mentioned"}, "verdict": "gray"},
	}}
	routerCall(t, newRequest("PUT", "/api/router/inbox-rules", custom)).Want(http.StatusOK).JSON(&set)
	var got state
	routerCall(t, newRequest("GET", "/api/router/inbox-rules", nil)).Want(http.StatusOK).JSON(&got)
	if got.Config == nil || len(got.Config.Rules) != 2 || got.Config.Rules[1].Verdict != "gray" {
		t.Fatalf("after custom rules = %+v", got.Config)
	}

	// A list stored past the API that no longer passes the check is shown as
	// invalid, not as no list.
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(settings, ARRAY[$2::text], $3::jsonb) WHERE id = $1`,
		testWorkspaceID, agentrouter.WorkspaceSettingInboxRules,
		`{"rules":[{"name":"comment","types":["new_comment"],"verdict":"gray"}]}`)
	var invalid state
	routerCall(t, newRequest("GET", "/api/router/inbox-rules", nil)).Want(http.StatusOK).JSON(&invalid)
	if invalid.Config != nil || !strings.Contains(invalid.ConfigError, "not a gray type") {
		t.Fatalf("with an invalid stored list = %+v", invalid)
	}
	routerCall(t, newRequest("PUT", "/api/router/inbox-rules", custom)).Want(http.StatusOK).JSON(&set)
	if set.Config == nil || set.ConfigError != "" {
		t.Fatalf("after setting it again = %+v", set)
	}

	// The report answers members and agents, with the definition of acting.
	var rep agentrouter.RulesReport
	routerCall(t, newRequest("GET", "/api/router/inbox-rules/report?since=48h&window=12h", nil)).Want(http.StatusOK).JSON(&rep)
	if rep.Definition == "" || rep.Window != "12h0m0s" || rep.Rules == nil || rep.Verdicts == nil {
		t.Fatalf("report = %+v", rep)
	}
	routerCall(t, asAgent(newRequest("GET", "/api/router/inbox-rules/report", nil), f.agentA, f.taskA)).Want(http.StatusOK)
	for _, q := range []string{"window=soon", "window=10m", "since=-1h", "since=1000h", "since=6h"} {
		routerCall(t, newRequest("GET", "/api/router/inbox-rules/report?"+q, nil)).Want(http.StatusBadRequest)
	}

	var off state
	routerCall(t, newRequest("PUT", "/api/router/inbox-rules", map[string]any{"off": true})).Want(http.StatusOK).JSON(&off)
	if off.Config != nil {
		t.Fatalf("after off = %+v", off)
	}
}
