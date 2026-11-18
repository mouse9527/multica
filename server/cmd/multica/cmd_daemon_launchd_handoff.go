// This file belongs to the mouse9527/multica fork (not present upstream).
package main

// Binary-update restarts under the daemon's own macOS LaunchAgent.
//
// Upstream's portable handoff spawns a Setsid successor and exits 0. Under
// launchd that detaches the runtime from its job: launchd records the job as
// not running, `launchctl bootout` no longer stops the daemon, and the
// successor runs whatever executable the restart target resolved to. Under
// our own LaunchAgent the daemon instead exits with
// daemonLaunchdHandoffExitStatus, and the plist's KeepAlive
// {SuccessfulExit=false} makes launchd start ProgramArguments again — the
// same executable path, now holding the new binary.
//
// KeepAlive restarts on any non-zero exit, not only the handoff status:
// launchd has no per-status filter. `multica daemon stop` and logout end the
// daemon with exit 0, so both stay final; a failing start is retried at
// launchd's ThrottleInterval (10s by default).
//
// Detection relies on the environment of the job launchd actually loaded,
// not on the plist file: a refreshed plist only takes effect when launchd
// loads the job again (next login, or bootout + bootstrap), so a job still
// running from an older plist — no KeepAlive — must keep the spawn handoff.
// The marker is unset right after it is read, so agent CLIs and daemons
// started from their shells (which inherit XPC_SERVICE_NAME) never see it.

import (
	"os"
	"strings"
)

const (
	// launchAgentSupervisorEnv carries the LaunchAgent's label in the
	// plist's EnvironmentVariables.
	launchAgentSupervisorEnv = "MULTICA_DAEMON_LAUNCH_AGENT"
	// daemonLaunchdHandoffExitStatus only needs to be non-zero for
	// KeepAlive {SuccessfulExit=false}; it reuses the systemd status so a
	// handoff reads the same in either supervisor's records.
	daemonLaunchdHandoffExitStatus = daemonSystemdHandoffExitStatus
)

// takeLaunchAgentSupervision reports whether this process was started by the
// profile's own LaunchAgent, then removes the marker from the environment.
// Call it once, early in the foreground daemon. launchd sets
// XPC_SERVICE_NAME to the job's label, so both must name this profile's
// label.
func takeLaunchAgentSupervision(profile string) bool {
	marker := os.Getenv(launchAgentSupervisorEnv)
	_ = os.Unsetenv(launchAgentSupervisorEnv)
	label := launchAgentLabel(profile)
	return marker == label && os.Getenv("XPC_SERVICE_NAME") == label
}

// writeLaunchAgentKeepAlive renders the restart policy the handoff relies on.
func writeLaunchAgentKeepAlive(b *strings.Builder) {
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n")
	b.WriteString("\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n")
	b.WriteString("\t</dict>\n")
}
