// Added by the mouse9527/multica fork: a system agent's prompt in a Super Agent chat turn (M-250).

package service

import (
	"strings"
	"testing"
)

// Evolution's rules that stop a Super Agent from reading and relaying issues.
var evolutionSuperAgentConflicts = []string{
	"Outside an analysis issue, do not scan",
	"Work only from what a member gives you directly",
	"When you are invoked for something outside improvement work",
}

// An agent's own prompt never mentions Super Agents: that content belongs to
// the chat layer (M-250).
func TestSystemPromptsCarryNoSuperAgentContent(t *testing.T) {
	for name, prompt := range map[string]string{
		"mika":      ComposeMikaInstructions("Mika", "notes"),
		"evolution": ComposeEvolutionInstructions("Evolution", "notes"),
	} {
		for _, banned := range []string{"Super Agent", "multica router"} {
			if strings.Contains(prompt, banned) {
				t.Errorf("%s's own prompt mentions %q", name, banned)
			}
		}
	}
}

// In a Super Agent chat turn, Evolution runs on its identity and the
// workspace's notes, without the task rules that conflict with the role.
func TestSuperAgentTurnInstructionsEvolution(t *testing.T) {
	full := EvolutionSystemInstructions("Evo")
	for _, rule := range evolutionSuperAgentConflicts {
		if !strings.Contains(full, rule) {
			t.Fatalf("Evolution's prompt no longer has %q; update this test with the rule's new wording", rule)
		}
	}

	got, ok := SuperAgentTurnInstructions(EvolutionSystemKey, "Evo", "")
	if !ok {
		t.Fatal("Evolution must run on its identity in a Super Agent turn")
	}
	if !strings.HasPrefix(got, "You are Evo, the workspace's Evolution agent") || !strings.HasPrefix(full, got) {
		t.Errorf("want the lead section of Evolution's prompt, got\n%s", got)
	}
	if strings.Contains(got, "\n## ") || strings.Contains(got, "{{AGENT_NAME}}") {
		t.Errorf("identity carries a section or a placeholder:\n%s", got)
	}
	for _, rule := range evolutionSuperAgentConflicts {
		if strings.Contains(got, rule) {
			t.Errorf("identity carries the rule %q", rule)
		}
	}

	const notes = "Answer in Chinese."
	got, _ = SuperAgentTurnInstructions(EvolutionSystemKey, "Evo", "  "+notes+"\n")
	if !strings.HasSuffix(got, superAgentWorkspaceNotesSection+"\n\n"+notes) {
		t.Errorf("notes must follow the identity under their section, got\n%s", got)
	}
	if strings.Contains(got, "guardrails") {
		t.Errorf("the notes section must not refer to guardrails that are not there:\n%s", got)
	}
}

// Every other agent keeps its own instructions in a Super Agent turn.
func TestSuperAgentTurnInstructionsOthersUnchanged(t *testing.T) {
	for _, key := range []string{"", MikaSystemKey, "agent_builder:x"} {
		if got, ok := SuperAgentTurnInstructions(key, "x", "notes"); ok || got != "" {
			t.Errorf("system key %q: got (%q, %v)", key, got, ok)
		}
	}
}

func TestBuiltinIdentity(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"You are X.\n\nMore lead.\n\n## Rules\n\n- a\n\n## More\n", "You are X.\n\nMore lead."},
		{"You are X. No sections.\n", "You are X. No sections."},
		{"## Rules\n\n- a", ""},
		{"You are X. Inline ## is not a heading.", "You are X. Inline ## is not a heading."},
	} {
		if got := builtinIdentity(c.in); got != c.want {
			t.Errorf("builtinIdentity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
