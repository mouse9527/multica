// This file belongs to the mouse9527/multica fork (not present upstream).
package main

// Log sinks for a foreground daemon whose standard streams were discarded.
//
// launchd starts a LaunchAgent that sets no StandardOutPath/StandardErrorPath
// with stdout and stderr on /dev/null. /dev/null is a character device, which
// logger.StderrIsTerminal reads as a terminal, so the daemon logged to
// /dev/null instead of the rotating daemon.log. A daemon whose stderr is the
// null device therefore takes the same sinks as `daemon start`'s background
// child: slog goes through the rotating daemon.log, and raw stdout/stderr
// (Go panics, fatal runtime errors) go to the bounded daemon.err.log.
//
// The plist deliberately keeps no StandardOutPath/StandardErrorPath: launchd
// would append to those paths for the life of the job, beyond the reach of
// the rotator, and refreshes rewrite the plist from the template anyway.

import (
	"os"
	"path/filepath"

	logger_pkg "github.com/multica-ai/multica/server/internal/logger"
)

// daemonLogsToTerminal reports whether the foreground daemon should keep its
// colored terminal logging. When stderr is the null device it first re-points
// every discarded standard stream at the profile's daemon.err.log.
func daemonLogsToTerminal(profile string) bool {
	if !isNullDevice(os.Stderr) {
		return logger_pkg.StderrIsTerminal()
	}
	errLogPath := daemonStderrLogPathForProfile(profile)
	// Best effort: a daemon that cannot keep crash output still runs, and
	// its slog output still reaches the rotating daemon.log.
	if err := os.MkdirAll(filepath.Dir(errLogPath), 0o755); err == nil {
		_ = redirectNullStdioToErrLog(errLogPath, os.Stdout, os.Stderr)
	}
	return false
}
