// This file belongs to the mouse9527/multica fork (not present upstream).
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func recordUpdates(d *Daemon) *[]string {
	var targets []string
	d.runUpdateFn = func(target string) (string, error) {
		targets = append(targets, target)
		return "updated", nil
	}
	return &targets
}

func TestTryAutoUpdate_FollowsLatestForkRelease(t *testing.T) {
	cases := []struct {
		current, latest string
	}{
		{"0.8.2-mouse.9", "v0.8.2-mouse.10"},
		{"0.8.2-mouse.10", "v0.8.3-mouse.1"},
		{"0.8.2", "v0.8.2-mouse.1"}, // an upstream build moves onto the fork
	}
	for _, c := range cases {
		t.Run(c.current+"->"+c.latest, func(t *testing.T) {
			d, restartCalls := newAutoUpdateTestDaemon(t, c.current)
			targets := recordUpdates(d)
			withStubRelease(t, &cli.GitHubRelease{TagName: c.latest}, nil)

			d.tryAutoUpdate(context.Background())

			if len(*targets) != 1 || (*targets)[0] != c.latest || restartCalls.Load() != 1 {
				t.Fatalf("targets=%v restarts=%d, want one update to %s", *targets, restartCalls.Load(), c.latest)
			}
		})
	}
}

func TestTryAutoUpdate_ForkAlreadyLatestOrNewer(t *testing.T) {
	for _, current := range []string{"0.8.2-mouse.10", "0.8.3-mouse.1"} {
		t.Run(current, func(t *testing.T) {
			d, restartCalls := newAutoUpdateTestDaemon(t, current)
			targets := recordUpdates(d)
			withStubRelease(t, &cli.GitHubRelease{TagName: "v0.8.2-mouse.10"}, nil)

			d.tryAutoUpdate(context.Background())

			if len(*targets) != 0 || restartCalls.Load() != 0 {
				t.Fatalf("updated from %s: targets=%v", current, *targets)
			}
		})
	}
}

// A failed download (network error, checksum mismatch, missing asset) leaves
// the daemon on its binary, able to claim tasks, and not restarting.
func TestTryAutoUpdate_ForkUpdateFailureKeepsRunning(t *testing.T) {
	d, restartCalls := newAutoUpdateTestDaemon(t, "0.8.2-mouse.1")
	withStubRelease(t, &cli.GitHubRelease{TagName: "v0.8.2-mouse.2"}, nil)
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

func TestTryAutoUpdate_ForkReleaseFetchFailure(t *testing.T) {
	d, restartCalls := newAutoUpdateTestDaemon(t, "0.8.2-mouse.1")
	withStubRelease(t, nil, errors.New("release API returned 502"))

	d.tryAutoUpdate(context.Background())

	if restartCalls.Load() != 0 || d.updating.Load() || d.pauseClaims {
		t.Fatalf("state changed after a failed release fetch")
	}
}

func TestAutoUpdateLoop_SkipsForkDevBuild(t *testing.T) {
	d := &Daemon{cfg: Config{AutoUpdateEnabled: true, CLIVersion: "v0.8.2-mouse.1-3-gabc1234"}, logger: slog.Default()}
	withStubRelease(t, &cli.GitHubRelease{TagName: "v0.8.2-mouse.2"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Returns at once: the pull half is disabled for a dev build and the
	// reload half is off in this config.
	d.autoUpdateLoop(ctx)
	if isReleaseVersion(d.cfg.CLIVersion) {
		t.Fatalf("fork dev build treated as a release")
	}
	if !isReleaseVersion("0.8.2-mouse.1") {
		t.Fatalf("fork release not treated as a release")
	}
}
