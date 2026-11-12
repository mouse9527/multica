// This file belongs to the mouse9527/multica fork (not present upstream).
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func withStubServerVersion(t *testing.T, version string, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	prev := fetchServerVersion
	fetchServerVersion = func(context.Context, *Client) (string, error) {
		calls.Add(1)
		return version, err
	}
	t.Cleanup(func() { fetchServerVersion = prev })
	return &calls
}

func withNoLatestRelease(t *testing.T) {
	t.Helper()
	prev := fetchLatestRelease
	fetchLatestRelease = func() (*cli.GitHubRelease, error) {
		t.Fatalf("server-aligned auto-update must not read the latest release")
		return nil, nil
	}
	t.Cleanup(func() { fetchLatestRelease = prev })
}

func newServerAlignedTestDaemon(t *testing.T, current string) (*Daemon, *atomic.Int32, *[]string) {
	t.Helper()
	d, restartCalls := newAutoUpdateTestDaemon(t, current)
	d.cfg.AutoUpdateTarget = AutoUpdateTargetServer
	var targets []string
	d.runUpdateFn = func(target string) (string, error) {
		targets = append(targets, target)
		return "updated", nil
	}
	return d, restartCalls, &targets
}

func TestTryAutoUpdate_ServerTarget_UpdatesToServerVersion(t *testing.T) {
	d, restartCalls, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
	withStubServerVersion(t, "v0.8.1-fork.2", nil)
	withNoLatestRelease(t)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 1 || (*targets)[0] != "v0.8.1-fork.2" {
		t.Fatalf("runUpdateFn targets = %v, want [v0.8.1-fork.2]", *targets)
	}
	if restartCalls.Load() != 1 {
		t.Fatalf("restart calls = %d, want 1", restartCalls.Load())
	}
}

func TestTryAutoUpdate_ServerTarget_AddsTagPrefix(t *testing.T) {
	d, _, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
	withStubServerVersion(t, "0.8.1-fork.3", nil)
	withNoLatestRelease(t)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 1 || (*targets)[0] != "v0.8.1-fork.3" {
		t.Fatalf("runUpdateFn targets = %v, want [v0.8.1-fork.3]", *targets)
	}
}

func TestTryAutoUpdate_ServerTarget_AlreadyAligned(t *testing.T) {
	d, restartCalls, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.2")
	withStubServerVersion(t, "v0.8.1-fork.2", nil)
	withNoLatestRelease(t)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 0 || restartCalls.Load() != 0 {
		t.Fatalf("aligned daemon updated: targets=%v restarts=%d", *targets, restartCalls.Load())
	}
}

// Alignment goes both ways: a runner ahead of the server moves back to the
// server's tag, because the point is that both run the same build.
func TestTryAutoUpdate_ServerTarget_FollowsServerDowngrade(t *testing.T) {
	d, _, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.3")
	withStubServerVersion(t, "v0.8.1-fork.2", nil)
	withNoLatestRelease(t)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 1 || (*targets)[0] != "v0.8.1-fork.2" {
		t.Fatalf("runUpdateFn targets = %v, want [v0.8.1-fork.2]", *targets)
	}
}

func TestTryAutoUpdate_ServerTarget_StaysWhenServerHasNoReleaseVersion(t *testing.T) {
	for _, serverVersion := range []string{"", "dev", "v0.8.1-fork.2-4-gabc1234"} {
		t.Run(serverVersion, func(t *testing.T) {
			d, restartCalls, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
			withStubServerVersion(t, serverVersion, nil)
			withNoLatestRelease(t)

			d.tryAutoUpdate(context.Background())

			if len(*targets) != 0 || restartCalls.Load() != 0 {
				t.Fatalf("updated toward non-release server version %q: targets=%v", serverVersion, *targets)
			}
		})
	}
}

func TestTryAutoUpdate_ServerTarget_StaysWhenServerUnreachable(t *testing.T) {
	d, restartCalls, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
	withStubServerVersion(t, "", errors.New("connection refused"))
	withNoLatestRelease(t)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 0 || restartCalls.Load() != 0 {
		t.Fatalf("updated without a server version: targets=%v", *targets)
	}
	if d.updating.Load() || d.pauseClaims {
		t.Fatalf("flags held after a failed server version fetch")
	}
}

// A failed download (network error, checksum mismatch, missing asset) must
// leave the daemon on its current binary and able to claim tasks, and must
// not restart it.
func TestTryAutoUpdate_ServerTarget_FailureKeepsRunning(t *testing.T) {
	d, restartCalls, _ := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
	withStubServerVersion(t, "v0.8.1-fork.2", nil)
	withNoLatestRelease(t)
	d.runUpdateFn = func(string) (string, error) {
		return "", errors.New("verify download: checksum mismatch")
	}

	d.tryAutoUpdate(context.Background())

	if restartCalls.Load() != 0 {
		t.Fatalf("restart after a failed update")
	}
	if d.updating.Load() || d.pauseClaims {
		t.Fatalf("updating=%v pauseClaims=%v after a failed update; want both cleared", d.updating.Load(), d.pauseClaims)
	}
}

func TestTryAutoUpdate_ServerTarget_DefersWhileTasksRun(t *testing.T) {
	d, restartCalls, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
	calls := withStubServerVersion(t, "v0.8.1-fork.2", nil)
	d.activeTasks.Store(1)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 0 || restartCalls.Load() != 0 || calls.Load() != 0 {
		t.Fatalf("auto-update ran with a task active: targets=%v fetches=%d", *targets, calls.Load())
	}
}

func TestTryAutoUpdate_LatestTarget_FollowsForkTags(t *testing.T) {
	d, restartCalls, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.1")
	d.cfg.AutoUpdateTarget = AutoUpdateTargetLatest
	withStubRelease(t, &cli.GitHubRelease{TagName: "v0.8.1-fork.2"}, nil)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 1 || (*targets)[0] != "v0.8.1-fork.2" || restartCalls.Load() != 1 {
		t.Fatalf("targets=%v restarts=%d, want one update to v0.8.1-fork.2", *targets, restartCalls.Load())
	}
}

func TestTryAutoUpdate_LatestTarget_NeverDowngrades(t *testing.T) {
	d, _, targets := newServerAlignedTestDaemon(t, "0.8.1-fork.2")
	d.cfg.AutoUpdateTarget = AutoUpdateTargetLatest
	withStubRelease(t, &cli.GitHubRelease{TagName: "v0.8.1-fork.1"}, nil)

	d.tryAutoUpdate(context.Background())

	if len(*targets) != 0 {
		t.Fatalf("latest target downgraded: %v", *targets)
	}
}

func TestAutoUpdateLoop_SkipsForkDevBuild(t *testing.T) {
	d := &Daemon{cfg: Config{AutoUpdateEnabled: true, CLIVersion: "v0.8.1-fork.1-3-gabc1234"}, logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Returns immediately: the pull half is disabled for a dev build and the
	// reload half is off in this config.
	d.autoUpdateLoop(ctx)
	if isReleaseVersion(d.cfg.CLIVersion) {
		t.Fatalf("fork dev build treated as a release")
	}
	if !isReleaseVersion("0.8.1-fork.1") {
		t.Fatalf("fork release not treated as a release")
	}
}

func TestFetchServerVersionReadsConfigEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server_version":" v0.8.1-fork.2 ","other":1}`))
	}))
	defer srv.Close()

	got, err := fetchServerVersion(context.Background(), NewClient(srv.URL))
	if err != nil {
		t.Fatalf("fetchServerVersion() error = %v", err)
	}
	if got != "v0.8.1-fork.2" {
		t.Fatalf("server version = %q", got)
	}
}

func TestAutoUpdateTargetFromEnv(t *testing.T) {
	for value, want := range map[string]string{"": "server", "server": "server", " Latest ": "latest"} {
		t.Setenv(autoUpdateTargetEnv, value)
		got, err := autoUpdateTargetFromEnv()
		if err != nil || got != want {
			t.Fatalf("autoUpdateTargetFromEnv(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	t.Setenv(autoUpdateTargetEnv, "newest")
	if _, err := autoUpdateTargetFromEnv(); err == nil {
		t.Fatalf("invalid target accepted")
	}
}

func TestLoadConfig_AutoUpdateTarget(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv(autoUpdateTargetEnv, "")
	cfg, err := LoadConfig(Overrides{ServerURL: "http://localhost:8080", WorkspacesRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpdateTarget != AutoUpdateTargetServer {
		t.Fatalf("AutoUpdateTarget = %q, want server", cfg.AutoUpdateTarget)
	}

	t.Setenv(autoUpdateTargetEnv, "bogus")
	if _, err := LoadConfig(Overrides{ServerURL: "http://localhost:8080", WorkspacesRoot: t.TempDir()}); err == nil {
		t.Fatalf("LoadConfig accepted an invalid auto-update target")
	}
}
