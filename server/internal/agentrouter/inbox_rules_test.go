// Modified for the mouse9527/multica fork: Router inbox S3b (M-133).

package agentrouter

import (
	"errors"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func ruleItem(typ, severity, details string) db.CreateInboxItemParams {
	return db.CreateInboxItemParams{Type: typ, Severity: severity, Details: []byte(details)}
}

func TestClassifyInboxItemDefaultRules(t *testing.T) {
	cfg, err := DefaultInboxRules([]string{"started_2"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name          string
		item          db.CreateInboxItemParams
		verdict, rule string
	}{
		// Gray types are gray whatever else matches.
		{"mention", ruleItem("mentioned", "info", `{}`), VerdictGray, "mention"},
		{"task failed", ruleItem("task_failed", "action_required", `{}`), VerdictGray, "failure"},
		{"agent blocked", ruleItem("agent_blocked", "info", `{}`), VerdictGray, "failure"},
		// enter_waiting_status: hit and miss.
		{"into the waiting status", ruleItem("status_changed", "info", `{"from":"in_progress","to":"started_2"}`), VerdictOwner, "enter_waiting_status"},
		{"into it, any case", ruleItem("status_changed", "info", `{"from":"in_progress","to":"STARTED_2"}`), VerdictOwner, "enter_waiting_status"},
		{"into another status", ruleItem("status_changed", "info", `{"from":"started_2","to":"in_review"}`), VerdictHandle, "status_progress"},
		// action_required: hit and miss.
		{"assigned to the owner", ruleItem("issue_assigned", "action_required", `{}`), VerdictOwner, "action_required"},
		{"assigned, info only", ruleItem("issue_assigned", "info", `{}`), VerdictOwner, RuleUnmatched},
		// The handle rules, each hit.
		{"priority", ruleItem("priority_changed", "info", `{}`), VerdictHandle, "field_update"},
		{"start date", ruleItem("start_date_changed", "info", `{}`), VerdictHandle, "field_update"},
		{"due date", ruleItem("due_date_changed", "info", `{}`), VerdictHandle, "field_update"},
		{"assignee changed", ruleItem("assignee_changed", "info", `{}`), VerdictHandle, "assignment_churn"},
		{"unassigned", ruleItem("unassigned", "info", `{}`), VerdictHandle, "assignment_churn"},
		{"subscribed", ruleItem("issue_subscribed", "info", `{}`), VerdictHandle, "subscribed"},
		{"comment", ruleItem("new_comment", "info", `{"comment_id":"x"}`), VerdictHandle, "comment"},
		// No rule: the owner gets it.
		{"new issue", ruleItem("issue_created", "info", `{}`), VerdictOwner, RuleUnmatched},
		{"children done", ruleItem("children_done", "info", `{}`), VerdictOwner, RuleUnmatched},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ClassifyInboxItem(cfg, c.item)
			if err != nil {
				t.Fatal(err)
			}
			if got.Verdict != c.verdict || got.Rule != c.rule {
				t.Fatalf("got %+v, want %s by %s", got, c.verdict, c.rule)
			}
		})
	}
}

func TestClassifyInboxItemGrayTypeWithoutGrayRule(t *testing.T) {
	cfg := InboxRulesConfig{Rules: []InboxRule{
		{Name: "anything_urgent", Severities: []string{"action_required"}, Verdict: VerdictOwner},
	}}
	got, err := ClassifyInboxItem(cfg, ruleItem("task_failed", "action_required", `{}`))
	if err != nil || got.Verdict != VerdictGray || got.Rule != RuleGrayDefault {
		t.Fatalf("got %+v (%v), want gray by %s: a gray type is never decided by an owner rule", got, err, RuleGrayDefault)
	}
}

func TestClassifyInboxItemFailsWithoutTheStatus(t *testing.T) {
	cfg := DefaultInboxRules([]string{"started_2"})
	for _, details := range []string{`not json`, `{"from":"todo"}`} {
		if got, err := ClassifyInboxItem(cfg, ruleItem("status_changed", "info", details)); err == nil {
			t.Fatalf("details %s: got %+v, want an error", details, got)
		}
	}
	// A rule that does not need the status never reads the details.
	if got, err := ClassifyInboxItem(cfg, ruleItem("new_comment", "info", `not json`)); err != nil || got.Rule != "comment" {
		t.Fatalf("got %+v (%v)", got, err)
	}
}

func TestInboxRulesNormalizeRejects(t *testing.T) {
	for name, c := range map[string]struct {
		rules []InboxRule
		want  string
	}{
		"handle without types":      {[]InboxRule{{Name: "all", Verdict: VerdictHandle}}, "must name its types"},
		"gray type handled":         {[]InboxRule{{Name: "m", Types: []string{"mentioned"}, Verdict: VerdictHandle}}, "gray type"},
		"gray type to the owner":    {[]InboxRule{{Name: "f", Types: []string{"Task_Failed"}, Verdict: VerdictOwner}}, "gray type"},
		"gray rule on a comment":    {[]InboxRule{{Name: "c", Types: []string{"new_comment"}, Verdict: VerdictGray}}, "not a gray type"},
		"gray rule, mixed types":    {[]InboxRule{{Name: "m", Types: []string{"mentioned", "Status_Changed"}, Verdict: VerdictGray}}, "not a gray type"},
		"gray rule without types":   {[]InboxRule{{Name: "g", Severities: []string{"action_required"}, Verdict: VerdictGray}}, "must name its types"},
		"unknown verdict":           {[]InboxRule{{Name: "x", Types: []string{"new_comment"}, Verdict: "drop"}}, "verdict must be"},
		"bad name":                  {[]InboxRule{{Name: "Bad Name", Types: []string{"new_comment"}, Verdict: VerdictHandle}}, "name must be"},
		"reserved name":             {[]InboxRule{{Name: RuleUnmatched, Types: []string{"new_comment"}, Verdict: VerdictOwner}}, "name must be"},
		"duplicate name":            {[]InboxRule{{Name: "a", Types: []string{"x"}, Verdict: VerdictOwner}, {Name: "a", Types: []string{"y"}, Verdict: VerdictOwner}}, "duplicate"},
		"to_statuses off a status":  {[]InboxRule{{Name: "a", Types: []string{"new_comment"}, ToStatuses: []string{"done"}, Verdict: VerdictOwner}}, "to_statuses"},
		"to_statuses on a wildcard": {[]InboxRule{{Name: "a", ToStatuses: []string{"done"}, Verdict: VerdictOwner}}, "to_statuses"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := InboxRulesConfig{Rules: c.rules}.Normalize()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
	got, err := InboxRulesConfig{Rules: []InboxRule{{Name: " gray_x ", Types: []string{" Mentioned ", "mentioned", ""}, Verdict: " GRAY "}}}.Normalize()
	if err != nil || got.Rules[0].Name != "gray_x" || len(got.Rules[0].Types) != 1 || got.Rules[0].Types[0] != "mentioned" || got.Rules[0].Verdict != VerdictGray {
		t.Fatalf("normalized = %+v (%v)", got, err)
	}
}

func TestParseInboxRulesConfig(t *testing.T) {
	if c := ParseInboxRulesConfig([]byte(`{"other":1}`)); c.Active() {
		t.Fatalf("no key: %+v", c)
	}
	// No list, or one removed (null), is off without an error; a stored list
	// that fails the check is off with ErrInboxRulesInvalid.
	for _, settings := range []string{`{"other":1}`, `{"router_inbox_rules":null}`} {
		if c, err := parseInboxRulesConfig([]byte(settings)); c.Active() || err != nil {
			t.Fatalf("%s: %+v (%v), want off without an error", settings, c, err)
		}
	}
	for _, settings := range []string{
		`{"router_inbox_rules":{"rules":[{"name":"c","types":["new_comment"],"verdict":"gray"}]}}`,
		`{"router_inbox_rules":{"rules":"all"}}`,
	} {
		if c, err := parseInboxRulesConfig([]byte(settings)); c.Active() || !errors.Is(err, ErrInboxRulesInvalid) {
			t.Fatalf("%s: %+v (%v), want off with ErrInboxRulesInvalid", settings, c, err)
		}
	}
	if c := ParseInboxRulesConfig([]byte(`{"router_inbox_rules":{"rules":[{"name":"m","types":["mentioned"],"verdict":"handle"}]}}`)); c.Active() {
		t.Fatalf("an invalid stored list must be off, got %+v", c)
	}
	c := ParseInboxRulesConfig([]byte(`{"router_inbox_rules":{"rules":[{"name":"c","types":["new_comment"],"verdict":"handle"}]}}`))
	if !c.Active() || c.Rules[0].Name != "c" {
		t.Fatalf("got %+v", c)
	}
}

func TestDecisionNoteRoundTrip(t *testing.T) {
	v := InboxVerdict{Verdict: VerdictHandle, Rule: "status_progress"}
	if note := v.DecisionNote(); note != "rule:handle:status_progress" {
		t.Fatalf("note = %q", note)
	}
	if got, ok := ParseDecisionNote(v.DecisionNote()); !ok || got != v {
		t.Fatalf("parsed %+v %v", got, ok)
	}
	for _, note := range []string{"", "released by agent", "rule:", "rule:handle", "rule::x"} {
		if _, ok := ParseDecisionNote(note); ok {
			t.Fatalf("%q parsed as a verdict", note)
		}
	}
}
