// Added by the mouse9527/multica fork: Evolution P6 (M-140).

package evolution

import (
	"strings"
	"testing"
	"time"
)

func TestMeasureInstructionsCountsRepeatedLines(t *testing.T) {
	text := strings.Join([]string{
		"# Rules",
		"- Always reply in the member's language.",
		"",
		"1. Always reply in the MEMBER'S   language.", // same line, other marker and case
		"Close the issue only after the PR is merged.",
		"> close the issue only after the pr is merged.",
		"ok",  // too short to compare
		"```", // a fence is not a line of text
		"Run the tests before you push a branch.",
	}, "\n")
	m := MeasureInstructions(text)
	if m.Lines != 5 || m.RepeatedLines != 2 || m.RepeatedPercent != 40 {
		t.Fatalf("measure = %+v, want 5 lines, 2 repeated, 40%%", m)
	}
	if m.Runes != len([]rune(text)) {
		t.Errorf("runes = %d, want %d", m.Runes, len([]rune(text)))
	}
	if got := MeasureInstructions(""); got != (InstructionsMeasure{}) {
		t.Errorf("empty instructions measure %+v", got)
	}
}

func TestInstructionsMeasureExceeds(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name string
		m    InstructionsMeasure
		want bool
	}{
		{"short and varied", InstructionsMeasure{Runes: 500, Lines: 10, RepeatedLines: 1, RepeatedPercent: 10}, false},
		{"long", InstructionsMeasure{Runes: cfg.CondenseMinRunes}, true},
		{"repetitive", InstructionsMeasure{Runes: 500, Lines: 10, RepeatedLines: 2, RepeatedPercent: 20}, true},
		// One repeated line out of three is a high share but no pattern.
		{"one repeat", InstructionsMeasure{Runes: 200, Lines: 3, RepeatedLines: 1, RepeatedPercent: 33}, false},
	}
	for _, tc := range cases {
		if got := tc.m.Exceeds(cfg); got != tc.want {
			t.Errorf("%s: exceeds = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCondensePeriod(t *testing.T) {
	cfg := DefaultConfig() // 03:00 Asia/Shanghai, day 1
	cfg.CondenseDayOfMonth = 5
	cases := []struct {
		now    string
		period string
		due    bool
	}{
		{"2026-11-04T23:00:00Z", "2026-11-01", true},  // 07:00 on the 5th in Shanghai
		{"2026-11-04T18:59:00Z", "2026-11-01", false}, // 02:59 on the 5th
		{"2026-11-30T16:30:00Z", "2026-12-01", false}, // already December in Shanghai
		{"2026-12-20T00:00:00Z", "2026-12-01", true},
	}
	for _, tc := range cases {
		now, _ := time.Parse(time.RFC3339, tc.now)
		period, due := condensePeriod(now, cfg)
		if period.Format(time.DateOnly) != tc.period || due != tc.due {
			t.Errorf("%s: period %s due %v, want %s %v", tc.now, period.Format(time.DateOnly), due, tc.period, tc.due)
		}
	}
}

func TestConfigCondenseDefaultsAndBounds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	if !cfg.CondenseWanted() {
		t.Fatal("the condense review is on by default while the analysis is on")
	}
	cfg.CondenseDisabled = true
	if cfg.CondenseWanted() {
		t.Error("condense_disabled must turn the review off")
	}
	cfg = DefaultConfig()
	cfg.CondenseDayOfMonth, cfg.CondenseMinRunes, cfg.CondenseRepeatPercent = 0, 0, 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a client without the P6 fields must still save: %v", err)
	}
	if n := cfg.normalized(); n.CondenseDayOfMonth != DefaultCondenseDayOfMonth || n.CondenseMinRunes != DefaultCondenseMinRunes || n.CondenseRepeatPercent != DefaultCondenseRepeatPercent {
		t.Errorf("defaults not filled: %+v", n)
	}
	for _, edit := range []func(c *Config){
		func(c *Config) { c.CondenseDayOfMonth = MaxCondenseDayOfMonth + 1 },
		func(c *Config) { c.CondenseMinRunes = MinCondenseMinRunes - 1 },
		func(c *Config) { c.CondenseRepeatPercent = MaxCondenseRepeatPercent + 1 },
	} {
		c := DefaultConfig()
		edit(&c)
		if !IsInvalidConfig(c.Validate()) {
			t.Errorf("out of bounds accepted: %+v", c)
		}
	}
	stored := ConfigFromSettings([]byte(`{"evolution": {"enabled": true, "condense_day_of_month": 31, "condense_min_runes": 5, "condense_repeat_percent": 100}}`))
	if stored.CondenseDayOfMonth != MaxCondenseDayOfMonth || stored.CondenseMinRunes != MinCondenseMinRunes || stored.CondenseRepeatPercent != MaxCondenseRepeatPercent {
		t.Errorf("stored values not clamped: %+v", stored)
	}
}

func condenseProposal() ProposalRequest {
	p := validProposal()
	p.Evidence = nil
	p.Topic = "anything"
	return p
}

func TestProposalValidateCondense(t *testing.T) {
	p := condenseProposal()
	if err := p.ValidateCondense(); err != nil {
		t.Fatalf("valid condense proposal rejected: %v", err)
	}
	if p.Topic != CondenseTopic {
		t.Errorf("topic = %q, want the server's %q", p.Topic, CondenseTopic)
	}
	cases := []struct {
		name string
		edit func(p *ProposalRequest)
		code string
	}{
		{"evidence", func(p *ProposalRequest) { p.Evidence = validProposal().Evidence }, RejectInvalid},
		{"another field", func(p *ProposalRequest) { p.Field = "description" }, RejectField},
		{"a workflow", func(p *ProposalRequest) { p.Target.Type, p.Field = TargetWorkflow, "steps" }, RejectField},
		{"a pure addition", func(p *ProposalRequest) { p.Change.Before = "" }, RejectInvalid},
		{"sensitive text", func(p *ProposalRequest) { p.Change.After = "Mail ops@example.com." }, RejectSensitive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := condenseProposal()
			tc.edit(&p)
			if got := rejectionCode(p.ValidateCondense()); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
	// An analysis run still needs evidence.
	p = condenseProposal()
	if got := rejectionCode(p.Validate()); got != RejectEvidence {
		t.Errorf("analysis proposal without evidence: %q", got)
	}
}

func TestProposalValidateTeam(t *testing.T) {
	p := validProposal()
	p.Target.Type, p.Field = TargetTeam, "merge_agents"
	p.Change = ProposalChange{After: "Merge the two release agents into one."}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid team proposal rejected: %v", err)
	}
	for _, field := range []string{"new_agent", "routing"} {
		q := p
		q.Field = field
		if err := q.Validate(); err != nil {
			t.Errorf("team field %s rejected: %v", field, err)
		}
	}
	q := p
	q.Field = "instructions"
	if got := rejectionCode(q.Validate()); got != RejectField {
		t.Errorf("team instructions: %q", got)
	}
	q = p
	q.Change.Before = "the current setup"
	if got := rejectionCode(q.Validate()); got != RejectInvalid {
		t.Errorf("team change.before: %q", got)
	}
	if got := rejectionCode(CheckApplyField(TargetTeam, "merge_agents")); got != RejectTarget {
		t.Errorf("apply must refuse team proposals: %q", got)
	}
}

func TestRenderCondenseAndTeamProposals(t *testing.T) {
	p := condenseProposal()
	if err := p.ValidateCondense(); err != nil {
		t.Fatal(err)
	}
	snap := TargetSnapshot{Type: TargetAgent, ID: testTargetID, Name: "Builder", Fields: map[string]string{"instructions": "x"}}
	m := &CondenseCandidate{AgentID: testTargetID, Name: "Builder", InstructionsMeasure: InstructionsMeasure{Runes: 7000, Lines: 40, RepeatedLines: 8, RepeatedPercent: 20}}
	body := proposalView{lang: "en", p: &p, snap: snap, runIssueID: testRunIssue, runIdentifier: "HAN-9", loc: time.UTC,
		measure: m, proposedRunes: 5000, cfg: DefaultConfig()}.render()
	for _, want := range []string{"7000 characters; of 40 lines, 8 (20%)", "After this change: 5000 characters", "condense review [HAN-9]"} {
		if !strings.Contains(body, want) {
			t.Errorf("condense body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Carried out by hand") {
		t.Errorf("an agent proposal is applied, not carried out by hand:\n%s", body)
	}

	team := validProposal()
	team.Target.Type, team.Field = TargetTeam, "new_agent"
	team.Change = ProposalChange{After: "Add an agent that triages new issues."}
	snap = TargetSnapshot{Type: TargetTeam, ID: testTargetID, Name: "Workspace", Fields: map[string]string{"new_agent": ""}}
	body = renderProposal("zh", &team, snap, resolved(team), testRunIssue, "HAN-9", time.UTC)
	for _, want := range []string{"+ Add an agent that triages new issues.", "需手动执行", "## 证据"} {
		if !strings.Contains(body, want) {
			t.Errorf("team body lacks %q:\n%s", want, body)
		}
	}
}
