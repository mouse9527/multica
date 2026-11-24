// Added by the mouse9527/multica fork: a system agent's prompt in a Super Agent chat turn (M-250).

package service

import "strings"

// An agent's prompt says what the agent is for; a chat only uses an agent to
// answer; a Super Agent is a chat made into one. So the Super Agent's role and
// rules come from the chat layer, and a system agent whose built-in task rules
// would stop it from doing that role leaves them out of its turns there.
//
// The built-in prompt is split by its own structure rather than by a second,
// hand-written prompt per agent: its lead section — everything before the
// first "## " heading — is the agent's identity, and the sections after it
// are its task rules. Keeping that shape is the only thing a system agent's
// INSTRUCTIONS.md has to do for this to work.
//
// superAgentIdentityOnly lists the system agents whose task rules conflict
// with the Super Agent role. Evolution's do: it reads no comments or issues
// outside its analysis runs and turns away anything that is not improvement
// work, while a Super Agent reads issues to relay them. Mika's working model
// (route work to issues, confirm before risky operations) fits the role, so
// Mika keeps its whole prompt.
var superAgentIdentityOnly = map[string]bool{
	EvolutionSystemKey: true,
}

const superAgentWorkspaceNotesSection = `## Workspace notes

Workspace notes below add this team's context and preferences. Follow them ahead of your own defaults.

Added by this workspace's admins:`

// SuperAgentTurnInstructions returns the agent's own instructions for a turn
// in a chat a Super Agent is bound to, and false when they are the same as in
// any other task. For a system agent listed in superAgentIdentityOnly that is
// its identity and the workspace's notes, without its task rules; the chat
// layer appends the Super Agent note after it.
func SuperAgentTurnInstructions(systemKey, displayName, workspaceNotes string) (string, bool) {
	if !superAgentIdentityOnly[systemKey] {
		return "", false
	}
	system, ok := ForkSystemInstructions(systemKey, displayName)
	if !ok {
		return "", false
	}
	identity := builtinIdentity(system)
	notes := strings.TrimSpace(workspaceNotes)
	if notes == "" {
		return identity, true
	}
	return identity + "\n\n" + superAgentWorkspaceNotesSection + "\n\n" + notes, true
}

// builtinIdentity is the lead section of a built-in prompt: the text before
// its first "## " heading, or all of it when it has none.
func builtinIdentity(system string) string {
	if strings.HasPrefix(system, "## ") {
		return ""
	}
	if i := strings.Index(system, "\n## "); i >= 0 {
		system = system[:i]
	}
	return strings.TrimSpace(system)
}
