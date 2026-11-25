// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

const leanBriefTestAgents = "11111111-2222-3333-4444-555555555555, 66666666-7777-8888-9999-000000000000"

func leanBriefTestSpec() autostartSpec {
	return autostartSpec{
		Exe:     "/opt/homebrew/bin/multica",
		Args:    autostartArgs(""),
		PathEnv: "/usr/bin",
	}
}

// launchAgentPlistBeforeM265 is the plist the default spec rendered before
// M-265. Without MULTICA_BRIEF_LEAN_AGENTS the output must stay exactly this.
const launchAgentPlistBeforeM265 = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ManagedBy</key>
	<string>multica-daemon-autostart</string>
	<key>Label</key>
	<string>ai.multica.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>/opt/homebrew/bin/multica</string>
		<string>daemon</string>
		<string>start</string>
		<string>--foreground</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
	<key>PATH</key>
	<string>/usr/bin</string>
	<key>MULTICA_DAEMON_LAUNCH_AGENT</key>
	<string>ai.multica.daemon</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Standard</string>
</dict>
</plist>
`

func leanBriefPlistEntry(value string) string {
	return "\t<key>" + execenv.LeanBriefAgentsEnv + "</key>\n\t<string>" + value + "</string>\n"
}

func TestLaunchAgentPlistWithoutLeanBriefIsUnchanged(t *testing.T) {
	for _, v := range []string{"", "  "} {
		t.Setenv(execenv.LeanBriefAgentsEnv, v)
		got := launchAgentPlistContent(leanBriefTestSpec(), launchAgentLabel(""))
		if got != launchAgentPlistBeforeM265 {
			t.Errorf("%s=%q: plist changed:\n got:\n%s\nwant:\n%s",
				execenv.LeanBriefAgentsEnv, v, got, launchAgentPlistBeforeM265)
		}
	}
}

func TestLaunchAgentPlistRecordsLeanBriefAgents(t *testing.T) {
	t.Setenv(execenv.LeanBriefAgentsEnv, leanBriefTestAgents)
	got := launchAgentPlistContent(leanBriefTestSpec(), launchAgentLabel(""))

	// The only difference is the one entry, inside EnvironmentVariables,
	// ahead of the LaunchAgent marker.
	marker := "\t<key>" + launchAgentSupervisorEnv + "</key>\n"
	want := strings.Replace(launchAgentPlistBeforeM265, marker,
		leanBriefPlistEntry(leanBriefTestAgents)+marker, 1)
	if got != want {
		t.Errorf("plist with %s:\n got:\n%s\nwant:\n%s", execenv.LeanBriefAgentsEnv, got, want)
	}

	// Without a PATH snapshot and with a ProcessType override the entry is
	// still written once, next to the other carried values.
	spec := leanBriefTestSpec()
	spec.PathEnv = ""
	spec.ProcessType = "Background"
	got = launchAgentPlistContent(spec, launchAgentLabel("dev"))
	if n := strings.Count(got, leanBriefPlistEntry(leanBriefTestAgents)); n != 1 {
		t.Errorf("entry appears %d times, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "<key>"+launchAgentProcessTypeEnv+"</key>") {
		t.Errorf("ProcessType override must still be recorded:\n%s", got)
	}
}

func TestLaunchAgentPlistEscapesLeanBriefAgents(t *testing.T) {
	t.Setenv(execenv.LeanBriefAgentsEnv, "a&b<c")
	got := launchAgentPlistContent(leanBriefTestSpec(), launchAgentLabel(""))
	if !strings.Contains(got, leanBriefPlistEntry("a&amp;b&lt;c")) {
		t.Errorf("value must be XML-escaped:\n%s", got)
	}
}

// TestSyncDaemonAutostartKeepsLeanBriefAgents pins the `daemon start`
// refresh: a daemon that has the variable rewrites the plist with it, one
// that does not rewrites it without.
func TestSyncDaemonAutostartKeepsLeanBriefAgents(t *testing.T) {
	orig := daemonExecutable
	daemonExecutable = func() (string, error) { return "/usr/local/bin/multica", nil }
	t.Cleanup(func() { daemonExecutable = orig })

	syncAndRender := func(t *testing.T) (content string, writes int) {
		t.Helper()
		t.Setenv("MULTICA_LAUNCHED_BY", "")
		t.Setenv(launchAgentProcessTypeEnv, "")
		stubPlatform(t, stubAutostartPlatform{
			supported:      true,
			refreshAllowed: true,
			read: func(string) (autostartState, error) {
				return autostartState{Present: true, Enabled: true, Managed: true}, nil
			},
			write: func(profile string, spec autostartSpec) (autostartState, bool, error) {
				writes++
				content = launchAgentPlistContent(spec, launchAgentLabel(profile))
				return autostartState{Enabled: true, Managed: true}, true, nil
			},
		})
		syncDaemonAutostart("", false)
		return content, writes
	}

	t.Run("set", func(t *testing.T) {
		t.Setenv(execenv.LeanBriefAgentsEnv, leanBriefTestAgents)
		content, writes := syncAndRender(t)
		if writes != 1 {
			t.Fatalf("writeAutostart calls = %d, want 1", writes)
		}
		if !strings.Contains(content, leanBriefPlistEntry(leanBriefTestAgents)) {
			t.Errorf("refreshed plist must keep %s:\n%s", execenv.LeanBriefAgentsEnv, content)
		}
	})

	t.Run("unset", func(t *testing.T) {
		t.Setenv(execenv.LeanBriefAgentsEnv, "")
		content, writes := syncAndRender(t)
		if writes != 1 {
			t.Fatalf("writeAutostart calls = %d, want 1", writes)
		}
		if strings.Contains(content, execenv.LeanBriefAgentsEnv) {
			t.Errorf("refreshed plist must not mention %s:\n%s", execenv.LeanBriefAgentsEnv, content)
		}
	})
}

// TestLeanBriefAgentsLeaveOtherAutostartEntriesAlone pins that the systemd
// unit, the XDG entry and the Windows Run value render the same with and
// without the variable.
func TestLeanBriefAgentsLeaveOtherAutostartEntriesAlone(t *testing.T) {
	spec := leanBriefTestSpec()
	render := func() []string {
		return []string{
			systemdUnitContent(spec),
			xdgAutostartContent(spec),
			windowsAutostartCommand(spec),
		}
	}

	t.Setenv(execenv.LeanBriefAgentsEnv, "")
	without := render()
	t.Setenv(execenv.LeanBriefAgentsEnv, leanBriefTestAgents)
	with := render()
	for i := range without {
		if with[i] != without[i] {
			t.Errorf("entry %d changed with %s set:\n got:\n%s\nwant:\n%s",
				i, execenv.LeanBriefAgentsEnv, with[i], without[i])
		}
	}
}
