// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

package service

import (
	"strings"
	"testing"
)

// TestComposeEvolutionInstructions covers the layering contract: the product
// half leads and is never replaced, workspace notes follow under their own
// heading, and empty notes add nothing.
func TestComposeEvolutionInstructions(t *testing.T) {
	system := EvolutionSystemInstructions("Evo")
	if !strings.HasPrefix(system, "You are Evo,") {
		t.Fatalf("system half should use the display name, got %q", firstLine(system))
	}
	if strings.Contains(system, mikaNamePlaceholder) {
		t.Fatal("the name placeholder was not substituted")
	}
	if got := EvolutionSystemInstructions("  "); !strings.HasPrefix(got, "You are "+EvolutionDefaultName+",") {
		t.Fatalf("blank name should fall back to the default, got %q", firstLine(got))
	}

	if got := ComposeEvolutionInstructions("Evo", "  \n"); got != system {
		t.Fatalf("empty notes must add nothing, got %q", got)
	}
	notes := "Prefer proposals about the review workflow."
	got := ComposeEvolutionInstructions("Evo", notes)
	if !strings.HasPrefix(got, system) {
		t.Fatal("the system half must lead the composed prompt")
	}
	if !strings.HasSuffix(got, notes) || !strings.Contains(got, "## Workspace notes") {
		t.Fatalf("notes must follow under their heading, got tail %q", got[len(system):])
	}
}

// TestForkSystemInstructions_OnlyFork guards the upstream hooks: they must
// change nothing for Mika or ordinary agents.
func TestForkSystemInstructions_OnlyFork(t *testing.T) {
	if _, ok := ForkSystemInstructions(EvolutionSystemKey, "Evo"); !ok {
		t.Fatal("evolution should have fork system instructions")
	}
	if got, ok := ComposeForkSystemInstructions(EvolutionSystemKey, "Evo", "n"); !ok || got != ComposeEvolutionInstructions("Evo", "n") {
		t.Fatalf("compose for evolution = %q, %v", got, ok)
	}
	for _, key := range []string{"", MikaSystemKey, "agent_builder:abc"} {
		if _, ok := ForkSystemInstructions(key, "x"); ok {
			t.Errorf("key %q must not get fork system instructions", key)
		}
		if _, ok := ComposeForkSystemInstructions(key, "x", "notes"); ok {
			t.Errorf("key %q must not get a fork composed prompt", key)
		}
	}
}

func TestEvolutionSkillIsScopedToEvolution(t *testing.T) {
	const skill = "multica-evolution"
	if named(loadBuiltinSkills(""), skill) {
		t.Errorf("an ordinary agent receives %q", skill)
	}
	if named(loadBuiltinSkills(MikaSystemKey), skill) {
		t.Errorf("Mika receives %q", skill)
	}
	evo := loadBuiltinSkills(EvolutionSystemKey)
	if !named(evo, skill) {
		t.Errorf("Evolution does not receive %q", skill)
	}
	if !named(evo, PlatformSkillName) {
		t.Errorf("Evolution lost %q", PlatformSkillName)
	}
	if named(evo, "multica-onboarding") {
		t.Error("Evolution receives Mika's onboarding skill")
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
