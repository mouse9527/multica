//go:build !windows

// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func openDevNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestIsNullDevice(t *testing.T) {
	if !isNullDevice(openDevNull(t)) {
		t.Errorf("isNullDevice(%s) = false, want true", os.DevNull)
	}
	regular, err := os.Create(filepath.Join(t.TempDir(), "stderr.log"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer regular.Close()
	if isNullDevice(regular) {
		t.Error("isNullDevice(regular file) = true, want false")
	}
}

// TestDaemonLogsToTerminalNullStderr is the launchd case: stdout and stderr
// on /dev/null. The daemon must pick the rotating daemon.log (not the
// terminal logger) and keep raw stream output in daemon.err.log.
func TestDaemonLogsToTerminalNullStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cli.TaskConfigRootEnv, "")

	stdout, stderr := openDevNull(t), openDevNull(t)
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() { os.Stdout, os.Stderr = origStdout, origStderr })

	if daemonLogsToTerminal("") {
		t.Fatal("daemonLogsToTerminal with stderr on /dev/null = true, want false")
	}
	if _, err := stdout.WriteString("stdout line\n"); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := stderr.WriteString("panic: boom\n"); err != nil {
		t.Fatalf("write stderr: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(home, ".multica", "daemon.err.log"))
	if err != nil {
		t.Fatalf("read daemon.err.log: %v", err)
	}
	for _, want := range []string{"stdout line", "panic: boom"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("daemon.err.log = %q, want it to contain %q", got, want)
		}
	}
}

// TestDaemonLogsToTerminalFileStderr is the background-child and systemd
// case: stderr already goes somewhere real, so nothing is re-pointed.
func TestDaemonLogsToTerminalFileStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cli.TaskConfigRootEnv, "")

	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr.log"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer stderr.Close()
	origStderr := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = origStderr })

	if daemonLogsToTerminal("") {
		t.Fatal("daemonLogsToTerminal with stderr on a file = true, want false")
	}
	if _, err := os.Stat(filepath.Join(home, ".multica", "daemon.err.log")); !os.IsNotExist(err) {
		t.Errorf("daemon.err.log stat err = %v, want not exist", err)
	}
}

// TestLaunchAgentPlistHasNoStdioPaths guards the design: launchd appends to
// StandardOutPath/StandardErrorPath for the life of the job with no
// rotation, so daemon output must stay with the daemon's own rotating sinks.
func TestLaunchAgentPlistHasNoStdioPaths(t *testing.T) {
	content := launchAgentPlistContent(autostartSpec{
		Exe:  "/opt/homebrew/bin/multica",
		Args: autostartArgs(""),
	}, launchAgentLabel(""))
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(content, key) {
			t.Errorf("LaunchAgent plist contains %s:\n%s", key, content)
		}
	}
}
