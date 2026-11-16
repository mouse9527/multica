// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import (
	"strings"
	"testing"
)

func plistProcessTypeEntry(processType string) string {
	return "<key>ProcessType</key>\n\t<string>" + processType + "</string>"
}

func TestLaunchAgentPlistDefaultsToStandard(t *testing.T) {
	t.Parallel()

	spec := autostartSpec{
		Exe:     "/opt/homebrew/bin/multica",
		Args:    autostartArgs(""),
		PathEnv: "/usr/bin",
	}
	for _, processType := range []string{"", defaultLaunchAgentProcessType} {
		spec.ProcessType = processType
		content := launchAgentPlistContent(spec, launchAgentLabel(""))
		if !strings.Contains(content, plistProcessTypeEntry("Standard")) {
			t.Errorf("ProcessType %q: plist must render Standard:\n%s", processType, content)
		}
		if strings.Contains(content, "Background") {
			t.Errorf("ProcessType %q: plist must not mention Background:\n%s", processType, content)
		}
		// The default is not recorded in the environment: nothing to carry.
		if strings.Contains(content, launchAgentProcessTypeEnv) {
			t.Errorf("ProcessType %q: default must not be written to EnvironmentVariables:\n%s", processType, content)
		}
	}
}

func TestLaunchAgentPlistRecordsOverride(t *testing.T) {
	t.Parallel()

	spec := autostartSpec{
		Exe:         "/opt/homebrew/bin/multica",
		Args:        autostartArgs(""),
		PathEnv:     "/usr/bin",
		ProcessType: "Background",
	}
	content := launchAgentPlistContent(spec, launchAgentLabel(""))
	for _, want := range []string{
		plistProcessTypeEntry("Background"),
		"<key>PATH</key>\n\t<string>/usr/bin</string>",
		"<key>" + launchAgentProcessTypeEnv + "</key>\n\t<string>Background</string>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist missing %q:\n%s", want, content)
		}
	}
	if n := strings.Count(content, "<key>EnvironmentVariables</key>"); n != 1 {
		t.Errorf("EnvironmentVariables appears %d times, want 1:\n%s", n, content)
	}

	// Without a PATH snapshot the override still needs an environment dict.
	spec.PathEnv = ""
	content = launchAgentPlistContent(spec, launchAgentLabel(""))
	if !strings.Contains(content, "<key>EnvironmentVariables</key>") ||
		!strings.Contains(content, "<key>"+launchAgentProcessTypeEnv+"</key>") {
		t.Errorf("override without PATH must still be recorded:\n%s", content)
	}
	if strings.Contains(content, "<key>PATH</key>") {
		t.Errorf("empty PATH snapshot must not be written:\n%s", content)
	}
}

func TestResolveLaunchAgentProcessType(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want string
	}{
		{"", "Standard"},
		{"  ", "Standard"},
		{"Background", "Background"},
		{"background", "Background"},
		{" INTERACTIVE ", "Interactive"},
		{"adaptive", "Adaptive"},
		{"standard", "Standard"},
	} {
		t.Setenv(launchAgentProcessTypeEnv, tc.env)
		got, err := resolveLaunchAgentProcessType()
		if err != nil || got != tc.want {
			t.Errorf("env %q: resolve = %q, %v; want %q", tc.env, got, err, tc.want)
		}
	}

	t.Setenv(launchAgentProcessTypeEnv, "Turbo")
	if got, err := resolveLaunchAgentProcessType(); err == nil {
		t.Errorf("env Turbo: resolve = %q, want an error", got)
	} else if !strings.Contains(err.Error(), launchAgentProcessTypeEnv) {
		t.Errorf("error %q should name %s", err, launchAgentProcessTypeEnv)
	}
}

// TestSyncDaemonAutostartProcessType pins what the `daemon start` refresh
// writes: an upstream-era Background plist is rewritten as Standard, a
// configured override survives the rewrite, and an invalid value leaves the
// existing plist untouched.
func TestSyncDaemonAutostartProcessType(t *testing.T) {
	orig := daemonExecutable
	daemonExecutable = func() (string, error) { return "/usr/local/bin/multica", nil }
	t.Cleanup(func() { daemonExecutable = orig })

	syncAndRender := func(t *testing.T) (content string, writes int) {
		t.Helper()
		t.Setenv("MULTICA_LAUNCHED_BY", "")
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

	t.Run("default rewrites as Standard", func(t *testing.T) {
		t.Setenv(launchAgentProcessTypeEnv, "")
		content, writes := syncAndRender(t)
		if writes != 1 {
			t.Fatalf("writeAutostart calls = %d, want 1", writes)
		}
		if !strings.Contains(content, plistProcessTypeEntry("Standard")) {
			t.Errorf("refreshed plist must be Standard:\n%s", content)
		}
	})

	t.Run("configured Background is kept", func(t *testing.T) {
		t.Setenv(launchAgentProcessTypeEnv, "background")
		content, writes := syncAndRender(t)
		if writes != 1 {
			t.Fatalf("writeAutostart calls = %d, want 1", writes)
		}
		if !strings.Contains(content, plistProcessTypeEntry("Background")) {
			t.Errorf("refreshed plist must keep the configured Background:\n%s", content)
		}
		if !strings.Contains(content, "<key>"+launchAgentProcessTypeEnv+"</key>\n\t<string>Background</string>") {
			t.Errorf("override must be carried into the launchd environment:\n%s", content)
		}
	})

	t.Run("invalid value skips the rewrite", func(t *testing.T) {
		t.Setenv(launchAgentProcessTypeEnv, "Turbo")
		_, writes := syncAndRender(t)
		if writes != 0 {
			t.Fatalf("writeAutostart calls = %d, want 0 for an invalid ProcessType", writes)
		}
	})
}
