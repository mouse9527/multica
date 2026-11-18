// This file belongs to the mouse9527/multica fork (not present upstream).
package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReleaseRepoDefaultsToFork(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	got, err := ReleaseRepo()
	if err != nil || got != "mouse9527/multica" {
		t.Fatalf("ReleaseRepo() = %q, %v; want mouse9527/multica", got, err)
	}
}

func TestReleaseRepoOverride(t *testing.T) {
	t.Setenv(releaseRepoEnv, " multica-ai/multica ")
	got, err := ReleaseRepo()
	if err != nil || got != "multica-ai/multica" {
		t.Fatalf("ReleaseRepo() = %q, %v", got, err)
	}
}

func TestReleaseRepoRejectsMalformed(t *testing.T) {
	for _, value := range []string{"multica", "a/b/c", "../x", "owner/..", "owner/name?x=1", "owner/na me", "-owner/name"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(releaseRepoEnv, value)
			if got, err := ReleaseRepo(); err == nil {
				t.Fatalf("ReleaseRepo() = %q, want error", got)
			}
			if _, err := FetchLatestRelease(); err == nil {
				t.Fatalf("FetchLatestRelease() succeeded with malformed repo")
			}
		})
	}
}

func TestIsForkReleaseVersion(t *testing.T) {
	cases := map[string]bool{
		"0.8.1":                    true,
		"v0.8.1":                   true,
		"v0.8.1-mouse.1":           true,
		"0.8.1-mouse.12":           true,
		"":                         false,
		"dev":                      false,
		"v0.8.1-mouse":             false,
		"v0.8.1-mouse.1-3-gabc123": false, // git describe on a fork tag
		"v0.8.1-fork.1":            false,
		"v0.8.1-12-gabc123":        false,
		"v0.8.1-rc.1":              false,
		"v0.8":                     false,
		"v0.8.1.2":                 false,
	}
	for v, want := range cases {
		if got := IsForkReleaseVersion(v); got != want {
			t.Errorf("IsForkReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestCompareForkVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.8.3-mouse.1", "v0.8.2-mouse.10", 1},
		{"v0.8.2-mouse.10", "v0.8.2-mouse.9", 1},
		{"v0.8.2-mouse.1", "v0.8.2", 1},
		{"v0.8.2", "v0.8.1-mouse.9", 1},
		{"v0.8.1-mouse.1", "0.8.1", 1},
		{"v0.8.1-mouse.2", "v0.8.1-mouse.1", 1},
		{"v0.8.1-mouse.10", "v0.8.1-mouse.9", 1},
		{"v0.8.2", "v0.8.1-mouse.9", 1},
		{"v0.8.1-mouse.1", "0.8.1-mouse.1", 0},
		{"v0.8.1", "v0.8.1-mouse.1", -1},
	}
	for _, c := range cases {
		got, ok := CompareForkVersions(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("CompareForkVersions(%q, %q) = %d, %v; want %d", c.a, c.b, got, ok, c.want)
		}
	}
	if _, ok := CompareForkVersions("v0.8.1-mouse.1-3-gabc", "v0.8.1"); ok {
		t.Errorf("dev build must not be comparable")
	}
	if IsNewerForkVersion("v0.8.1-mouse.1", "dev") {
		t.Errorf("IsNewerForkVersion must stay on an unparseable current version")
	}
}

func TestBrewUpdateBlocked(t *testing.T) {
	prev := isBrewInstall
	t.Cleanup(func() { isBrewInstall = prev })

	isBrewInstall = func() bool { return false }
	t.Setenv(releaseRepoEnv, "")
	if err := BrewUpdateBlocked(); err != nil {
		t.Fatalf("non-brew install blocked: %v", err)
	}

	isBrewInstall = func() bool { return true }
	if err := BrewUpdateBlocked(); err == nil || !strings.Contains(err.Error(), "mouse9527/multica") {
		t.Fatalf("brew install following the fork must be blocked, got %v", err)
	}

	t.Setenv(releaseRepoEnv, "multica-ai/multica")
	if err := BrewUpdateBlocked(); err != nil {
		t.Fatalf("brew install following upstream must not be blocked: %v", err)
	}
}

// fakeReleaseServer serves the subset of the GitHub Releases API and asset
// downloads that UpdateViaDownload uses.
type fakeReleaseServer struct {
	*httptest.Server
	repo     string
	tag      string
	assets   map[string][]byte
	failures map[string]int // asset name -> HTTP status to return instead

	mu       sync.Mutex
	requests []string
}

func newFakeReleaseServer(t *testing.T, repo, tag string) *fakeReleaseServer {
	t.Helper()
	f := &fakeReleaseServer{repo: repo, tag: tag, assets: map[string][]byte{}, failures: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	t.Setenv(releaseAPIBaseURLEnv, f.URL+"/api")
	t.Setenv(releaseDownloadBaseURLEnv, "")
	return f
}

func (f *fakeReleaseServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	repoPrefix := "/api/repos/" + f.repo + "/releases/"
	switch {
	case r.URL.Path == repoPrefix+"latest" || r.URL.Path == repoPrefix+"tags/"+f.tag:
		release := GitHubRelease{TagName: f.tag}
		for name := range f.assets {
			release.Assets = append(release.Assets, GitHubReleaseAsset{
				Name:               name,
				BrowserDownloadURL: f.URL + "/download/" + f.tag + "/" + name,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(release)
	case strings.HasPrefix(r.URL.Path, "/download/"+f.tag+"/"):
		name := strings.TrimPrefix(r.URL.Path, "/download/"+f.tag+"/")
		if status := f.failures[name]; status != 0 {
			http.Error(w, "injected failure", status)
			return
		}
		data, ok := f.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReleaseServer) requested(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.requests {
		if p == path {
			return true
		}
	}
	return false
}

// addCLIArchive publishes this platform's archive containing binary and a
// checksums.txt; checksumOverride, when non-empty, replaces the real digest.
func (f *fakeReleaseServer) addCLIArchive(t *testing.T, binary []byte, checksumOverride string) string {
	t.Helper()
	version := strings.TrimPrefix(f.tag, "v")
	name := fmt.Sprintf("multica-cli-%s-%s-%s.%s", version, runtime.GOOS, runtime.GOARCH, releaseArchiveExtension(runtime.GOOS))
	archive := buildCLIArchive(t, binary)
	f.assets[name] = archive
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		digest = checksumOverride
	}
	f.assets[ChecksumManifestName] = []byte(fmt.Sprintf("%s  %s\n", digest, name))
	return name
}

func buildCLIArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("multica.exe")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(binary); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"LICENSE", []byte("license")}, {"multica", binary}} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o755, Size: int64(len(entry.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stageExecutable points UpdateViaDownload at a temp "installed" binary.
func stageExecutable(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "multica")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := updateExecutablePath
	updateExecutablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { updateExecutablePath = prev })
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFetchLatestReleaseReadsForkByDefault(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	f := newFakeReleaseServer(t, "mouse9527/multica", "v0.8.1-mouse.2")

	got, err := FetchLatestRelease()
	if err != nil {
		t.Fatalf("FetchLatestRelease() error = %v", err)
	}
	if got.TagName != "v0.8.1-mouse.2" {
		t.Fatalf("tag = %q", got.TagName)
	}
	if !f.requested("/api/repos/mouse9527/multica/releases/latest") {
		t.Fatalf("latest release was not read from the fork; requests: %v", f.requests)
	}
}

func TestUpdateViaDownloadInstallsNewRelease(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	f := newFakeReleaseServer(t, "mouse9527/multica", "v0.8.1-mouse.2")
	assetName := f.addCLIArchive(t, []byte("new binary"), "")
	exe := stageExecutable(t, "old binary")

	out, err := UpdateViaDownloadWithTimeout("v0.8.1-mouse.2", 5*time.Second)
	if err != nil {
		t.Fatalf("UpdateViaDownloadWithTimeout() error = %v", err)
	}
	if !strings.Contains(out, assetName) {
		t.Fatalf("output %q does not name %s", out, assetName)
	}
	if got := readFile(t, exe); got != "new binary" {
		t.Fatalf("binary content = %q, want new binary", got)
	}
	if !f.requested("/api/repos/mouse9527/multica/releases/tags/v0.8.1-mouse.2") {
		t.Fatalf("release metadata was not read by tag from the fork; requests: %v", f.requests)
	}
}

func TestUpdateViaDownloadKeepsBinaryWhenDownloadFails(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	f := newFakeReleaseServer(t, "mouse9527/multica", "v0.8.1-mouse.2")
	assetName := f.addCLIArchive(t, []byte("new binary"), "")
	f.failures[assetName] = http.StatusBadGateway
	exe := stageExecutable(t, "old binary")

	if _, err := UpdateViaDownloadWithTimeout("v0.8.1-mouse.2", 5*time.Second); err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("error = %v, want download failure", err)
	}
	if got := readFile(t, exe); got != "old binary" {
		t.Fatalf("binary replaced after a failed download: %q", got)
	}
	assertNoUpdateTempFiles(t, exe)
}

func TestUpdateViaDownloadKeepsBinaryOnChecksumMismatch(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	f := newFakeReleaseServer(t, "mouse9527/multica", "v0.8.1-mouse.2")
	f.addCLIArchive(t, []byte("tampered binary"), strings.Repeat("0", 64))
	exe := stageExecutable(t, "old binary")

	if _, err := UpdateViaDownloadWithTimeout("v0.8.1-mouse.2", 5*time.Second); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	if got := readFile(t, exe); got != "old binary" {
		t.Fatalf("binary replaced despite checksum mismatch: %q", got)
	}
	assertNoUpdateTempFiles(t, exe)
}

func TestUpdateViaDownloadFailsWithoutChecksumManifest(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	f := newFakeReleaseServer(t, "mouse9527/multica", "v0.8.1-mouse.2")
	f.addCLIArchive(t, []byte("new binary"), "")
	delete(f.assets, ChecksumManifestName)
	exe := stageExecutable(t, "old binary")

	if _, err := UpdateViaDownloadWithTimeout("v0.8.1-mouse.2", 5*time.Second); err == nil || !strings.Contains(err.Error(), ChecksumManifestName) {
		t.Fatalf("error = %v, want missing checksum manifest", err)
	}
	if got := readFile(t, exe); got != "old binary" {
		t.Fatalf("binary replaced without a checksum manifest: %q", got)
	}
}

func TestUpdateViaDownloadFailsWhenReleaseMissing(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	newFakeReleaseServer(t, "mouse9527/multica", "v0.8.1-mouse.2")
	exe := stageExecutable(t, "old binary")

	if _, err := UpdateViaDownloadWithTimeout("v0.8.1-mouse.3", 5*time.Second); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want release API 404", err)
	}
	if got := readFile(t, exe); got != "old binary" {
		t.Fatalf("binary replaced for a missing release: %q", got)
	}
}

func assertNoUpdateTempFiles(t *testing.T, exe string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(exe), "multica-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}
