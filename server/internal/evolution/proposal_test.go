// Added by the mouse9527/multica fork: Evolution P3 (M-137).

package evolution

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testTargetID  = "0195f0a0-0000-7000-8000-000000000001"
	testCommentA  = "0195f0a0-0000-7000-8000-0000000000a1"
	testCommentB  = "0195f0a0-0000-7000-8000-0000000000b1"
	testChatID    = "0195f0a0-0000-7000-8000-0000000000c1"
	testIssueID   = "0195f0a0-0000-7000-8000-0000000000d1"
	testRunIssue  = "0195f0a0-0000-7000-8000-0000000000e1"
	testIssueID2  = "0195f0a0-0000-7000-8000-0000000000d2"
	testChatSessn = "0195f0a0-0000-7000-8000-0000000000f1"
)

func validProposal() ProposalRequest {
	return ProposalRequest{
		Target:  ProposalTarget{Type: TargetAgent, ID: testTargetID},
		Field:   "instructions",
		Topic:   "closes issues before merge",
		Title:   "Close issues only after the PR is merged",
		Problem: "Issues were closed while their PRs were open.",
		Change:  ProposalChange{Before: "Close the issue when merged.", After: "Close the issue after checking the PR is merged."},
		Evidence: []ProposalEvidence{
			{Type: EvidenceComment, ID: testCommentA, Note: "closed early", Quote: "closed before the PR was merged"},
			{Type: EvidenceComment, ID: testCommentB},
		},
		ExpectedEffect: "Fewer reopened issues.",
		Risk:           "low",
		RiskReason:     "One sentence.",
		Rollback:       "Restore the sentence.",
	}
}

func rejectionCode(err error) string {
	var r *RejectedError
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func TestProposalValidate(t *testing.T) {
	p := validProposal()
	if err := p.Validate(); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	cases := []struct {
		name string
		edit func(p *ProposalRequest)
		code string
	}{
		{"target type", func(p *ProposalRequest) { p.Target.Type = "runtime" }, RejectTarget},
		{"target id", func(p *ProposalRequest) { p.Target.ID = "nope" }, RejectInvalid},
		{"forbidden field", func(p *ProposalRequest) { p.Field = "custom_env" }, RejectField},
		{"field of another target", func(p *ProposalRequest) { p.Field = "steps" }, RejectField},
		{"squad member field on an agent", func(p *ProposalRequest) { p.Field = "members" }, RejectField},
		{"no change", func(p *ProposalRequest) { p.Change.After = p.Change.Before }, RejectInvalid},
		{"no problem", func(p *ProposalRequest) { p.Problem = "  " }, RejectInvalid},
		{"risk", func(p *ProposalRequest) { p.Risk = "huge" }, RejectInvalid},
		{"multi-line title", func(p *ProposalRequest) { p.Title = "one\ntwo" }, RejectInvalid},
		{"no evidence", func(p *ProposalRequest) { p.Evidence = nil }, RejectEvidence},
		{"evidence twice", func(p *ProposalRequest) { p.Evidence[1] = p.Evidence[0] }, RejectInvalid},
		{"evidence type", func(p *ProposalRequest) { p.Evidence[1].Type = "run_log" }, RejectInvalid},
		{"chat quote", func(p *ProposalRequest) {
			p.Evidence[1] = ProposalEvidence{Type: EvidenceChat, ID: testChatID, Quote: "anything"}
		}, RejectQuote},
		{"issue quote", func(p *ProposalRequest) {
			p.Evidence[1] = ProposalEvidence{Type: EvidenceIssue, ID: testIssueID, Quote: "anything"}
		}, RejectQuote},
		{"long quote", func(p *ProposalRequest) { p.Evidence[0].Quote = strings.Repeat("长", MaxQuoteRunes+1) }, RejectQuote},
		{"email", func(p *ProposalRequest) { p.Problem += " Ask bob@example.com." }, RejectSensitive},
		{"token in the diff", func(p *ProposalRequest) { p.Change.After = "Use ghp_abcdefghijklmnopqrstuvwx." }, RejectSensitive},
		{"internal host in a note", func(p *ProposalRequest) { p.Evidence[1].Note = "see build.corp" }, RejectSensitive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProposal()
			tc.edit(&p)
			if got := rejectionCode(p.Validate()); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
	// A quote of exactly 120 characters is fine.
	p = validProposal()
	p.Evidence[0].Quote = strings.Repeat("长", MaxQuoteRunes)
	if err := p.Validate(); err != nil {
		t.Fatalf("120-character quote rejected: %v", err)
	}
}

func TestFingerprintIgnoresWording(t *testing.T) {
	a := Fingerprint(TargetAgent, testTargetID, "instructions", "Closes issues before merge")
	b := Fingerprint(TargetAgent, strings.ToUpper(testTargetID), "instructions", "  closes  issues, before MERGE! ")
	if a != b || len(a) != 32 {
		t.Fatalf("fingerprints differ: %q %q", a, b)
	}
	for _, other := range []string{
		Fingerprint(TargetAgent, testTargetID, "description", "Closes issues before merge"),
		Fingerprint(TargetWorkflow, testTargetID, "instructions", "Closes issues before merge"),
		Fingerprint(TargetAgent, testTargetID, "instructions", "Closes issues after merge"),
	} {
		if other == a {
			t.Fatal("a different target, field, or topic must change the fingerprint")
		}
	}
}

func TestChatFragmentFound(t *testing.T) {
	chat := "Please stop closing my issues before the pull request is merged."
	if !chatFragmentFound("A member said: please  STOP closing my issues before the PR.", chat) {
		t.Error("a 20-character stretch of the chat should be found, ignoring case and spacing")
	}
	if chatFragmentFound("A member asked to wait for the merge before closing.", chat) {
		t.Error("a summary must pass")
	}
	if !chatFragmentFound("他说别再提前关单了好吗。", "别再提前关单了好吗") {
		t.Error("a short chat message pasted whole should be found")
	}
	if !chatFragmentFound("they wrote: wait for merge", "wait for merge") {
		t.Error("a short message pasted whole should be found")
	}
	if chatFragmentFound("ok then", "ok") {
		t.Error("trivially short messages are not checked")
	}
}

func TestNameFound(t *testing.T) {
	cases := []struct {
		text, name string
		want       bool
	}{
		{"Ann keeps reopening issues", "Ann", true},
		{"ann's review", "Ann", true},
		{"Annotations are missing", "Ann", false},
		{"planning", "Ann", false},
		{"王小明提了三次", "王小明", true},
		{"anything", "A", false},
		{"Zelda Quartz asked", "Zelda Quartz", true},
	}
	for _, tc := range cases {
		if got := nameFound(tc.text, tc.name); got != tc.want {
			t.Errorf("nameFound(%q, %q) = %v, want %v", tc.text, tc.name, got, tc.want)
		}
	}
}

func resolved(p ProposalRequest) []ResolvedEvidence {
	return []ResolvedEvidence{
		{ProposalEvidence: p.Evidence[0], Context: "issue:" + testIssueID, Content: "It was closed before the PR was merged, again.", IssueID: testIssueID, IssueIdentifier: "HAN-1"},
		{ProposalEvidence: p.Evidence[1], Context: "issue:" + testIssueID2, Content: "Same here.", IssueID: testIssueID2, IssueIdentifier: "HAN-2"},
	}
}

func TestCheckEvidence(t *testing.T) {
	p := validProposal()
	if err := p.CheckEvidence(resolved(p), 2); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	if got := rejectionCode(p.CheckEvidence(resolved(p), 3)); got != RejectEvidence {
		t.Errorf("too few sources: %q", got)
	}
	sameIssue := resolved(p)
	sameIssue[1].Context = sameIssue[0].Context
	if got := rejectionCode(p.CheckEvidence(sameIssue, 2)); got != RejectEvidence {
		t.Errorf("two comments on one issue are one source: %q", got)
	}
	p.Evidence[0].Quote = "closed after the PR"
	if got := rejectionCode(p.CheckEvidence(resolved(p), 2)); got != RejectQuote {
		t.Errorf("made-up quote: %q", got)
	}
}

func TestCheckVerbatim(t *testing.T) {
	chat := VerbatimSource{Type: EvidenceChat, ID: testChatID, Content: "I need the agent to wait until the PR is merged, every time."}
	long := "The release checklist says to wait for CI, then tag, then publish the notes; " +
		"but the agent tagged before CI finished three times this week, and each time the tag had to be deleted and redone by hand."
	comment := VerbatimSource{Type: EvidenceComment, ID: testCommentA, Content: long}

	// Cited or merely served: the check is the same, on every published field.
	p := validProposal()
	p.Problem = "A member asked: I need the agent to wait until the PR is merged."
	if got := rejectionCode(p.CheckVerbatim([]VerbatimSource{chat})); got != RejectQuote {
		t.Errorf("pasted chat in problem: %q", got)
	}
	p = validProposal()
	p.Evidence[1].Note = "they said they need the agent to wait until the PR"
	if got := rejectionCode(p.CheckVerbatim([]VerbatimSource{chat})); got != RejectQuote {
		t.Errorf("pasted chat in a note: %q", got)
	}
	p = validProposal()
	p.Problem = "A member asked for agents to wait for merges."
	if err := p.CheckVerbatim([]VerbatimSource{chat}); err != nil {
		t.Errorf("summarized chat rejected: %v", err)
	}

	runes := []rune(long)
	p = validProposal()
	p.Problem = "As one comment put it: " + string(runes[10:10+MaxQuoteRunes+1])
	if got := rejectionCode(p.CheckVerbatim([]VerbatimSource{comment})); got != RejectQuote {
		t.Errorf("121 characters of a comment in problem: %q", got)
	}
	p = validProposal()
	p.Change.After = "Wait for CI.\n" + strings.ToUpper(string(runes[:MaxQuoteRunes+5]))
	if got := rejectionCode(p.CheckVerbatim([]VerbatimSource{comment})); got != RejectQuote {
		t.Errorf("a comment copied into the diff, case changed: %q", got)
	}
	p = validProposal()
	p.Problem = "As one comment put it: " + string(runes[10:10+MaxQuoteRunes])
	if err := p.CheckVerbatim([]VerbatimSource{comment}); err != nil {
		t.Errorf("120 characters of a comment rejected: %v", err)
	}
	// change.before is the target's own text, tied to it by CheckTarget.
	p = validProposal()
	p.Change.Before = long
	if err := p.CheckVerbatim([]VerbatimSource{comment}); err != nil {
		t.Errorf("change.before checked: %v", err)
	}
}

func TestCheckTarget(t *testing.T) {
	snap := TargetSnapshot{Type: TargetAgent, ID: testTargetID, Fields: map[string]string{
		"instructions": "Reply in English.\nClose the issue   when merged.",
	}}
	p := validProposal()
	if err := p.CheckTarget(snap); err != nil {
		t.Fatalf("faithful before rejected (spacing differs): %v", err)
	}
	p.Change.Before = "Close the issue when approved."
	if got := rejectionCode(p.CheckTarget(snap)); got != RejectStaleBefore {
		t.Errorf("stale before: %q", got)
	}
	p.Change.Before, p.Change.After = "", "Reply in English."
	if got := rejectionCode(p.CheckTarget(snap)); got != RejectStaleBefore {
		t.Errorf("adding what is already there: %q", got)
	}
	other := snap
	other.Fields = map[string]string{"instructions": "changed"}
	if snap.ContentHash() == other.ContentHash() {
		t.Error("the content hash must change with the content")
	}
}

func TestRenderProposal(t *testing.T) {
	p := validProposal()
	p.Change.After = "Use ``` fences\n\nwith a blank line."
	snap := TargetSnapshot{Type: TargetAgent, ID: testTargetID, Name: "Builder", Fields: map[string]string{"instructions": "x"}}
	body := renderProposal("zh", &p, snap, resolved(p), testRunIssue, "HAN-9", time.UTC)
	for _, want := range []string{
		"## 对象", "**Builder**", "## 改动", "````diff", "- Close the issue when merged.", "+ Use ``` fences", "\n+\n",
		"## 证据", "[HAN-1](mention://issue/" + testIssueID + ")", testCommentA, "“closed before the PR was merged”",
		"## 风险档位", "低 · One sentence.", "[HAN-9](mention://issue/" + testRunIssue + ")",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if en := renderProposal(proposalLanguage("Evolution"), &p, snap, resolved(p), testRunIssue, "HAN-9", time.UTC); !strings.Contains(en, "## Problem and cause") {
		t.Errorf("English body:\n%s", en)
	}
}

func TestConfigProposalDefaults(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxProposalsPerRun, cfg.MinEvidenceCount, cfg.RejectedCooldownDays = 0, 0, 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a client without the P3 fields must still save: %v", err)
	}
	if n := cfg.normalized(); n.MaxProposalsPerRun != DefaultProposalsPerRun || n.MinEvidenceCount != DefaultMinEvidence || n.RejectedCooldownDays != DefaultRejectedCooldownDays {
		t.Errorf("defaults not filled: %+v", n)
	}
	cfg.MaxProposalsPerRun = MaxProposalsPerRun + 1
	if !IsInvalidConfig(cfg.Validate()) {
		t.Error("over the bound must be rejected")
	}
	stored := ConfigFromSettings([]byte(`{"evolution": {"enabled": true, "max_proposals_per_run": 999, "min_evidence_count": -3}}`))
	if stored.MaxProposalsPerRun != MaxProposalsPerRun || stored.MinEvidenceCount != MinEvidenceLowerBound || stored.RejectedCooldownDays != DefaultRejectedCooldownDays {
		t.Errorf("stored values not clamped: %+v", stored)
	}
}
