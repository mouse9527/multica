// This file belongs to the mouse9527/multica fork (not present upstream).
package main

// LaunchAgent ProcessType for the daemon's macOS autostart entry.
//
// Upstream writes ProcessType=Background, which is right for a daemon that
// only polls and holds a websocket. Here the daemon is the parent of every
// agent workload (agent CLIs, builds, tests), and launchd applies the
// Background QoS clamp — efficiency cores, throttled CPU and I/O — to the
// whole process tree. The fork therefore defaults to Standard.
//
// MULTICA_DAEMON_PROCESS_TYPE overrides the default. A non-default choice is
// also written into the plist's EnvironmentVariables: the launchd-started
// daemon then sees the same value, so the refresh every `daemon start`
// performs (including binary-update re-execs) keeps writing it instead of
// reverting to the default. The plist only takes effect when launchd loads
// the job again (next login, or bootout + bootstrap); a re-exec does not.

import (
	"fmt"
	"os"
	"strings"
)

const (
	launchAgentProcessTypeEnv     = "MULTICA_DAEMON_PROCESS_TYPE"
	defaultLaunchAgentProcessType = "Standard"
)

// launchAgentProcessTypes are the values launchd.plist(5) accepts.
var launchAgentProcessTypes = []string{"Background", "Standard", "Adaptive", "Interactive"}

// resolveLaunchAgentProcessType reads MULTICA_DAEMON_PROCESS_TYPE, matching
// case-insensitively and returning launchd's canonical spelling. An unknown
// value is an error: `autostart enable` reports it, and the `daemon start`
// refresh skips the rewrite, leaving the existing plist as it is.
func resolveLaunchAgentProcessType() (string, error) {
	v := strings.TrimSpace(os.Getenv(launchAgentProcessTypeEnv))
	if v == "" {
		return defaultLaunchAgentProcessType, nil
	}
	for _, t := range launchAgentProcessTypes {
		if strings.EqualFold(v, t) {
			return t, nil
		}
	}
	return "", fmt.Errorf("%s=%q is not a launchd ProcessType (want one of %s)",
		launchAgentProcessTypeEnv, v, strings.Join(launchAgentProcessTypes, ", "))
}

// launchAgentProcessType is the ProcessType a spec renders; a spec built
// without one gets the fork default.
func launchAgentProcessType(spec autostartSpec) string {
	if spec.ProcessType == "" {
		return defaultLaunchAgentProcessType
	}
	return spec.ProcessType
}

// launchAgentProcessTypeOverridden reports whether the spec carries a
// non-default ProcessType that must be recorded in EnvironmentVariables.
func launchAgentProcessTypeOverridden(spec autostartSpec) bool {
	return launchAgentProcessType(spec) != defaultLaunchAgentProcessType
}
