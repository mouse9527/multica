// This file belongs to the mouse9527/multica fork (not present upstream).
//
// It points `multica update` and the daemon's auto-update at the fork's own
// GitHub Releases instead of multica-ai/multica, and teaches the version
// helpers about the fork's tag scheme: vX.Y.Z-fork.N, where X.Y.Z is the
// upstream release the fork is based on and N counts fork releases on top of
// it. Upstream's IsReleaseVersion / IsNewerVersion reject any suffix, so a
// fork build would otherwise look like a dev build and never self-update.
package cli

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/selfexec"
)

// DefaultReleaseRepo is the GitHub repository whose Releases the CLI updates
// from when MULTICA_RELEASE_REPO is unset.
const DefaultReleaseRepo = "mouse9527/multica"

// upstreamReleaseRepo is the repository the Homebrew tap formula follows.
const upstreamReleaseRepo = "multica-ai/multica"

// MULTICA_RELEASE_REPO selects the "owner/name" repository whose Releases
// `multica update` and the daemon read. Combine with
// MULTICA_RELEASE_API_BASE_URL to point at a GitHub API-compatible mirror.
const releaseRepoEnv = "MULTICA_RELEASE_REPO"

var releaseRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

// ReleaseRepo returns the configured release repository, or an error when
// MULTICA_RELEASE_REPO is set to something that is not "owner/name". A bad
// value fails closed: silently falling back to the default would hide a typo
// behind a working update from somewhere the operator did not ask for.
func ReleaseRepo() (string, error) {
	value := strings.TrimSpace(os.Getenv(releaseRepoEnv))
	if value == "" {
		return DefaultReleaseRepo, nil
	}
	if !releaseRepoPattern.MatchString(value) || strings.Contains(value, "..") {
		return "", fmt.Errorf("%s must look like owner/name, got %q", releaseRepoEnv, value)
	}
	return value, nil
}

// releaseRepoAPIPath returns "/repos/<owner>/<name>" for the configured repo.
func releaseRepoAPIPath() (string, error) {
	repo, err := ReleaseRepo()
	if err != nil {
		return "", err
	}
	return "/repos/" + repo, nil
}

// updateExecutablePath resolves the binary UpdateViaDownload replaces.
// Overridable so tests can swap a temp file instead of the test binary.
var updateExecutablePath = selfexec.Resolve

// isBrewInstall is overridable for tests.
var isBrewInstall = IsBrewInstall

// BrewUpdateBlocked returns an error when this binary was installed by
// Homebrew but updates are configured to come from a repository other than
// upstream. The tap formula tracks multica-ai/multica, so `brew upgrade`
// would move the binary off the configured repository, and overwriting a
// file inside the Cellar would leave brew's records lying about it.
func BrewUpdateBlocked() error {
	if !isBrewInstall() {
		return nil
	}
	repo, err := ReleaseRepo()
	if err != nil {
		return err
	}
	if repo == upstreamReleaseRepo {
		return nil
	}
	return fmt.Errorf("multica was installed by Homebrew, whose formula follows %s, but updates are configured to come from %s; "+
		"install the binary from https://github.com/%s/releases instead (see docs/fork-release.md)", upstreamReleaseRepo, repo, repo)
}

// forkVersionPattern accepts "X.Y.Z" and "X.Y.Z-fork.N", each with an
// optional "v". Anything else — a `git describe` dev suffix, "dev", an
// upstream pre-release — is not a release this updater may install.
var forkVersionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-fork\.(\d+))?$`)

// parseForkVersion returns {major, minor, patch, fork}; a plain upstream
// version has fork = 0, so v0.8.1 < v0.8.1-fork.1 < v0.8.1-fork.2 < v0.8.2.
func parseForkVersion(v string) ([4]int, bool) {
	m := forkVersionPattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [4]int{}, false
	}
	var out [4]int
	for i := 0; i < 4; i++ {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [4]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// IsForkReleaseVersion reports whether v is a tagged release in the fork's
// scheme (X.Y.Z or X.Y.Z-fork.N).
func IsForkReleaseVersion(v string) bool {
	_, ok := parseForkVersion(v)
	return ok
}

// CompareForkVersions returns -1, 0 or 1 comparing a with b, and false when
// either side is not a release version.
func CompareForkVersions(a, b string) (int, bool) {
	av, ok := parseForkVersion(a)
	if !ok {
		return 0, false
	}
	bv, ok := parseForkVersion(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 4; i++ {
		if av[i] != bv[i] {
			if av[i] > bv[i] {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

// IsNewerForkVersion reports whether latest is strictly newer than current.
// Unparseable input on either side means "stay on current".
func IsNewerForkVersion(latest, current string) bool {
	cmp, ok := CompareForkVersions(latest, current)
	return ok && cmp > 0
}
