// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLaunchAgentPlistKeepsAliveOnHandoff pins the plist half of the handoff
// contract: launchd restarts a non-zero exit (the handoff status) but not
// exit 0 (`daemon stop`, logout), and the job carries its label marker.
func TestLaunchAgentPlistKeepsAliveOnHandoff(t *testing.T) {
	t.Parallel()

	for _, profile := range []string{"", "staging"} {
		label := launchAgentLabel(profile)
		for _, pathEnv := range []string{"/usr/bin", ""} {
			spec := autostartSpec{Exe: "/opt/homebrew/bin/multica", Args: autostartArgs(profile), PathEnv: pathEnv}
			content := launchAgentPlistContent(spec, label)
			for _, want := range []string{
				"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>",
				"<key>EnvironmentVariables</key>",
				"<key>" + launchAgentSupervisorEnv + "</key>\n\t<string>" + label + "</string>",
			} {
				if !strings.Contains(content, want) {
					t.Errorf("profile %q PATH %q: plist missing %q:\n%s", profile, pathEnv, want, content)
				}
			}
			if n := strings.Count(content, "<key>EnvironmentVariables</key>"); n != 1 {
				t.Errorf("EnvironmentVariables appears %d times, want 1:\n%s", n, content)
			}
		}
	}
	if daemonLaunchdHandoffExitStatus == 0 {
		t.Fatal("the handoff status must be non-zero for KeepAlive {SuccessfulExit=false}")
	}
}

// TestLaunchAgentPlistLints runs the rendered plist through plutil where it
// exists, so the hand-written KeepAlive dict stays valid for launchd.
func TestLaunchAgentPlistLints(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not found")
	}
	spec := autostartSpec{Exe: "/opt/a&b/multica", Args: autostartArgs("team/dev"), PathEnv: "/usr/bin", ProcessType: "Background"}
	path := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(path, []byte(launchAgentPlistContent(spec, launchAgentLabel("team/dev"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
}

// TestTakeLaunchAgentSupervision covers both handoff branches' gate: only the
// profile's own LaunchAgent job hands the restart to launchd; a direct
// `daemon start`, an old plist without the marker, and processes that merely
// inherited launchd's environment keep the spawn handoff. The marker never
// survives the read.
func TestTakeLaunchAgentSupervision(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		marker  string
		xpc     string
		want    bool
	}{
		{name: "own LaunchAgent", marker: "ai.multica.daemon", xpc: "ai.multica.daemon", want: true},
		{name: "own LaunchAgent, named profile", profile: "staging", marker: "ai.multica.daemon.staging", xpc: "ai.multica.daemon.staging", want: true},
		{name: "direct daemon start", marker: "", xpc: "", want: false},
		{name: "job loaded from a plist without the marker", marker: "", xpc: "ai.multica.daemon", want: false},
		{name: "marker without launchd", marker: "ai.multica.daemon", xpc: "", want: false},
		{name: "started from a terminal", marker: "ai.multica.daemon", xpc: "application.com.mitchellh.ghostty.1", want: false},
		{name: "another profile's job", profile: "staging", marker: "ai.multica.daemon", xpc: "ai.multica.daemon", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(launchAgentSupervisorEnv, tc.marker)
			t.Setenv("XPC_SERVICE_NAME", tc.xpc)
			if got := takeLaunchAgentSupervision(tc.profile); got != tc.want {
				t.Errorf("takeLaunchAgentSupervision(%q) = %v, want %v", tc.profile, got, tc.want)
			}
			if _, ok := os.LookupEnv(launchAgentSupervisorEnv); ok {
				t.Errorf("%s must be cleared after the read", launchAgentSupervisorEnv)
			}
			// A second read, as a child or a re-check would see it, is false.
			if takeLaunchAgentSupervision(tc.profile) {
				t.Error("takeLaunchAgentSupervision = true after the marker was cleared")
			}
		})
	}
}
