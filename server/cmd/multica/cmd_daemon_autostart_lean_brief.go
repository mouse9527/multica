// This file belongs to the mouse9527/multica fork (not present upstream).
package main

// MULTICA_BRIEF_LEAN_AGENTS in the macOS LaunchAgent (M-265).
//
// The lean runtime brief (M-259, execenv/runtime_config_lean.go) is switched
// on per agent by MULTICA_BRIEF_LEAN_AGENTS in the daemon's environment. A
// daemon started by its LaunchAgent only has the plist's
// EnvironmentVariables, and every `daemon start` re-renders that plist from
// code, so a value added to the plist by hand was dropped at the next
// refresh. The variable is therefore carried like a non-default
// MULTICA_DAEMON_PROCESS_TYPE: when the rendering process has it, the plist
// records it, and the launchd-started daemon — which then has it too — keeps
// writing it on every refresh. Without it (unset or blank) nothing is
// written and the plist is byte-for-byte what it was before this file.
//
// The value is read from the process environment at render time rather than
// captured in autostartSpec, so the upstream file only carries the one-line
// call in launchAgentPlistContent. `autostart enable` and the `daemon start`
// refresh render in the process whose environment is meant, so both see the
// right value.
//
// The plist only takes effect when launchd loads the job again (next login,
// or bootout + bootstrap). systemd units, XDG entries and the Windows Run
// value are not changed.

import (
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// writeLaunchAgentLeanBriefEnv adds MULTICA_BRIEF_LEAN_AGENTS to the plist's
// EnvironmentVariables dict when the current process has a non-blank value.
// The value is written as is; the daemon parses it (execenv.leanBriefListed).
func writeLaunchAgentLeanBriefEnv(b *strings.Builder) {
	v := os.Getenv(execenv.LeanBriefAgentsEnv)
	if strings.TrimSpace(v) == "" {
		return
	}
	writePlistEntry(b, execenv.LeanBriefAgentsEnv, v)
}
