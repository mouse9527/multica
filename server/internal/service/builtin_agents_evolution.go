// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

package service

import (
	_ "embed"
	"strings"
)

// EvolutionSystemKey marks the workspace's built-in Evolution agent. Like
// MikaSystemKey it is the agent's identity for every server-side decision, and
// the row stays kind='user' for the same reasons.
const EvolutionSystemKey = "evolution"

// EvolutionDefaultName is the fallback display name for the prompt. The
// enable endpoint creates the agent under a localized name, and owners may
// rename it; nothing server-side keys off the name.
const EvolutionDefaultName = "Evolution"

// The system half of Evolution's prompt, shipped with the binary and layered
// over the workspace's notes at claim time, exactly as Mika's is.
//
//go:embed builtin_agents/evolution/INSTRUCTIONS.md
var evolutionSystemInstructions string

const evolutionWorkspaceNotesSection = `## Workspace notes

Workspace notes below add this team's context and preferences. Follow them ahead of your own defaults; they refine how you apply these instructions, and they do not remove the guardrails above.

Added by this workspace's admins:`

// EvolutionSystemInstructions returns the product-owned half of Evolution's
// prompt for an agent displayed under the given name.
func EvolutionSystemInstructions(displayName string) string {
	name := strings.TrimSpace(displayName)
	if name == "" {
		name = EvolutionDefaultName
	}
	return strings.ReplaceAll(
		strings.TrimRight(evolutionSystemInstructions, "\n"),
		mikaNamePlaceholder,
		name,
	)
}

// ComposeEvolutionInstructions layers the workspace's notes under the
// product-owned system instructions.
func ComposeEvolutionInstructions(displayName, workspaceNotes string) string {
	system := EvolutionSystemInstructions(displayName)
	notes := strings.TrimSpace(workspaceNotes)
	if notes == "" {
		return system
	}
	return system + "\n\n" + evolutionWorkspaceNotesSection + "\n\n" + notes
}

// ForkSystemInstructions returns the product-owned prompt of a fork-defined
// system agent, and false for every other system key (Mika included, which
// upstream code handles). It is the single entry point the upstream files call,
// so each of them carries one hook rather than one per fork agent.
func ForkSystemInstructions(systemKey, displayName string) (string, bool) {
	switch systemKey {
	case EvolutionSystemKey:
		return EvolutionSystemInstructions(displayName), true
	}
	return "", false
}

// ComposeForkSystemInstructions is ForkSystemInstructions with the
// workspace's notes layered under it, for the claim payload.
func ComposeForkSystemInstructions(systemKey, displayName, workspaceNotes string) (string, bool) {
	switch systemKey {
	case EvolutionSystemKey:
		return ComposeEvolutionInstructions(displayName, workspaceNotes), true
	}
	return "", false
}
