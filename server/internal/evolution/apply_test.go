// Added by the mouse9527/multica fork: Evolution P4 (M-138).

package evolution

import (
	"errors"
	"strings"
	"testing"
)

func TestPatchText(t *testing.T) {
	const text = "Always reply in English.\nClose the issue when the work is merged.\nKeep comments short."
	cases := []struct {
		name, current, before, after string
		want                         string
		matches                      int
	}{
		{"replaces the one match", text, "Close the issue when the work is merged.", "Close the issue after its PR is merged.",
			"Always reply in English.\nClose the issue after its PR is merged.\nKeep comments short.", 1},
		{"ignores how whitespace is laid out", text, "English.  Close the\tissue", "English. Close the",
			"Always reply in English. Close the when the work is merged.\nKeep comments short.", 1},
		{"regexp characters are text", "Use (a|b) only.", "(a|b)", "a",
			"Use a only.", 1},
		{"appends on its own line", "First.", "", "Second.", "First.\nSecond.", 1},
		{"appends after a trailing newline", "First.\n", "", "Second.", "First.\nSecond.", 1},
		{"fills an empty field", "", "", "Only.", "Only.", 1},
		{"no match", text, "Never written.", "x", "", 0},
		{"two matches", "Be brief. Be brief.", "Be brief.", "Be short.", "", 2},
		{"case matters", text, "close the issue", "x", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, n := PatchText(tc.current, tc.before, tc.after)
			if n != tc.matches {
				t.Fatalf("matches = %d, want %d", n, tc.matches)
			}
			if n == 1 && got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPatchedRefusesAmbiguousChanges: a filed proposal names one place in
// the field, so apply writes what was reviewed.
func TestPatchedRefusesAmbiguousChanges(t *testing.T) {
	p := validProposal()
	snap := TargetSnapshot{Type: TargetAgent, ID: testTargetID, Fields: map[string]string{
		"instructions": "Close the issue when merged.\nClose the issue when merged.",
	}}
	var rej *RejectedError
	if _, err := p.Patched(snap); !errors.As(err, &rej) || rej.Code != RejectAmbiguous {
		t.Fatalf("want %s, got %v", RejectAmbiguous, err)
	}
	snap.Fields["instructions"] = "Close the issue when merged."
	got, err := p.Patched(snap)
	if err != nil || got != p.Change.After {
		t.Fatalf("got %q, %v", got, err)
	}

	steps := validProposal()
	steps.Target.Type, steps.Field = TargetWorkflow, "steps"
	steps.Change = ProposalChange{Before: `"status_key": "todo"`, After: `"status_key": "todo",,`}
	snap = TargetSnapshot{Type: TargetWorkflow, Fields: map[string]string{"steps": "[\n  {\n    \"status_key\": \"todo\"\n  }\n]"}}
	if _, err := steps.Patched(snap); !errors.As(err, &rej) || rej.Code != RejectInvalid {
		t.Fatalf("broken steps JSON: want %s, got %v", RejectInvalid, err)
	}
}

func TestCheckApplyField(t *testing.T) {
	for _, ok := range [][2]string{
		{TargetAgent, "instructions"}, {TargetAgent, "description"}, {TargetAgent, "skills"},
		{TargetWorkflow, "steps"}, {TargetWorkflow, "name"}, {TargetWorkflow, "description"},
	} {
		if err := CheckApplyField(ok[0], ok[1]); err != nil {
			t.Errorf("%s %s: %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range []struct{ targetType, field, code string }{
		{TargetSquad, "instructions", RejectTarget},
		{TargetAgent, "custom_env", RejectField},
		{TargetAgent, "custom_args", RejectField},
		{TargetAgent, "mcp_config", RejectField},
		{TargetAgent, "runtime_id", RejectField},
		{TargetAgent, "permission_mode", RejectField},
		{TargetAgent, "model", RejectField},
		{TargetWorkflow, "instructions", RejectField},
	} {
		var rej *RejectedError
		if err := CheckApplyField(bad.targetType, bad.field); !errors.As(err, &rej) || rej.Code != bad.code {
			t.Errorf("%s %s: want %s, got %v", bad.targetType, bad.field, bad.code, err)
		}
	}
}

func TestSameFieldValue(t *testing.T) {
	if !SameFieldValue("skills", "b\na", "a\n b \n\n") {
		t.Error("skills compare as a set of names")
	}
	if SameFieldValue("skills", "a\nb", "a") {
		t.Error("a missing skill is a difference")
	}
	if !SameFieldValue("steps", `[{"status_key":"todo"}]`, "[\n  {\n    \"status_key\": \"todo\"\n  }\n]") {
		t.Error("steps compare as JSON values")
	}
	if SameFieldValue("steps", `[{"status_key":"todo"}]`, `[{"status_key":"done"}]`) {
		t.Error("different steps are a difference")
	}
	if SameFieldValue("instructions", "a ", "a") {
		t.Error("text compares exactly")
	}
}

func TestOnlyFieldChanged(t *testing.T) {
	before := TargetSnapshot{Fields: map[string]string{"instructions": "old", "description": "d", "skills": "a"}}
	after := TargetSnapshot{Fields: map[string]string{"instructions": "new", "description": "d", "skills": "a"}}
	if !OnlyFieldChanged(before, after, "instructions", "new") {
		t.Error("the field alone changed to the wanted value")
	}
	if OnlyFieldChanged(before, after, "instructions", "other") {
		t.Error("the field holds something else")
	}
	after.Fields["description"] = "changed too"
	if OnlyFieldChanged(before, after, "instructions", "new") {
		t.Error("another field changed")
	}
}

func TestActiveApply(t *testing.T) {
	if _, ok := ActiveApply(nil); ok {
		t.Fatal("no versions, nothing applied")
	}
	versions := []Version{
		{ID: "v1", Version: 1, Action: ActionApply},
		{ID: "v2", Version: 2, Action: ActionRollback, RevertedOf: "v1"},
	}
	if _, ok := ActiveApply(versions); ok {
		t.Fatal("a rolled-back apply is not active")
	}
	versions = append(versions, Version{ID: "v3", Version: 3, Action: ActionApply})
	if v, ok := ActiveApply(versions); !ok || v.ID != "v3" {
		t.Fatalf("got %+v %v, want v3", v, ok)
	}
}

func TestProposalNotesRender(t *testing.T) {
	for _, agentName := range []string{"Evolution", "进化"} {
		n := ProposalNote{AgentName: agentName, TargetType: TargetAgent, TargetName: "Reviewer", Field: "instructions", Version: 4, RevertedVersion: 3}
		for _, kind := range []string{StateApplied, StateRolledBack, StateStale} {
			got := n.render(kind)
			if strings.Contains(got, "%!") || !strings.Contains(got, "Reviewer") {
				t.Errorf("%s %s: %q", agentName, kind, got)
			}
		}
		if got := n.render(StateApplied); !strings.Contains(got, "4") || !strings.Contains(got, "instructions") {
			t.Errorf("%s applied: %q", agentName, got)
		}
		if got := n.render(StateRolledBack); !strings.Contains(got, "3") || !strings.Contains(got, "4") {
			t.Errorf("%s rolled back: %q", agentName, got)
		}
	}
}
