// This file belongs to the mouse9527/multica fork (not present upstream).
//
// It decides which release the daemon's auto-update installs. Upstream always
// follows the newest GitHub release; the fork defaults to the version the
// connected server reports, so a runner moves to the same tag as the server
// it talks to rather than ahead of it.
package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	// AutoUpdateTargetServer installs the release whose tag equals the
	// server's reported version (GET /api/config server_version), upgrading
	// or downgrading to match. Nothing happens while the server does not
	// report a release version (for example a `main` image built as "dev").
	AutoUpdateTargetServer = "server"
	// AutoUpdateTargetLatest installs the newest release of the release
	// repository, never downgrading. This is upstream's behavior and what an
	// empty Config.AutoUpdateTarget means.
	AutoUpdateTargetLatest = "latest"
)

const autoUpdateTargetEnv = "MULTICA_DAEMON_AUTO_UPDATE_TARGET"

// autoUpdateTargetFromEnv reads MULTICA_DAEMON_AUTO_UPDATE_TARGET, defaulting
// to AutoUpdateTargetServer.
func autoUpdateTargetFromEnv() (string, error) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(autoUpdateTargetEnv)))
	switch value {
	case "":
		return AutoUpdateTargetServer, nil
	case AutoUpdateTargetServer, AutoUpdateTargetLatest:
		return value, nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", autoUpdateTargetEnv, AutoUpdateTargetServer, AutoUpdateTargetLatest, value)
	}
}

// fetchServerVersion returns the server_version the server reports on
// /api/config ("" when the server does not report one). Overridable for tests.
var fetchServerVersion = func(ctx context.Context, c *Client) (string, error) {
	if c == nil {
		return "", fmt.Errorf("no server client")
	}
	var resp struct {
		ServerVersion string `json:"server_version"`
	}
	if err := c.getJSON(ctx, "/api/config", &resp); err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.ServerVersion), nil
}

// autoUpdateTarget returns the release tag tryAutoUpdate should install, and
// false when the daemon should stay on its current version this tick.
func (d *Daemon) autoUpdateTarget(ctx context.Context) (string, bool) {
	if d.cfg.AutoUpdateTarget == AutoUpdateTargetServer {
		serverVersion, err := fetchServerVersion(ctx, d.client)
		if err != nil {
			d.logger.Warn("auto-update: fetch server version failed — will retry", "error", err)
			return "", false
		}
		if !isReleaseVersion(serverVersion) {
			d.logger.Debug("auto-update: server does not report a release version — staying", "server_version", serverVersion)
			return "", false
		}
		if cmp, ok := cli.CompareForkVersions(serverVersion, d.cfg.CLIVersion); !ok || cmp == 0 {
			return "", false
		}
		return "v" + strings.TrimPrefix(serverVersion, "v"), true
	}

	release, err := fetchLatestRelease()
	if err != nil {
		d.logger.Warn("auto-update: fetch latest release failed — will retry", "error", err)
		return "", false
	}
	if release == nil || release.TagName == "" {
		return "", false
	}
	if !isNewerVersion(release.TagName, d.cfg.CLIVersion) {
		return "", false
	}
	return release.TagName, true
}
