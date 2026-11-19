// Added by the mouse9527/multica fork: resilient repo cache rebuilds (M-161).

package repocache

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// caseConflictFetchOutput is what Git 2.54 prints on macOS (APFS) when the
// remote has refs/heads/Dup and refs/heads/dup: it stores Dup, refuses dup,
// stores every other ref, and exits 1.
const caseConflictFetchOutput = `error: You're on a case-insensitive filesystem, and the remote you are
trying to fetch from has references that only differ in casing. It
is impossible to store such references with the 'files' backend. You
can either accept this as-is, in which case you won't be able to
store all remote references on disk. Or you can alternatively
migrate your repository to use the 'reftable' backend with the
following command:

    git refs migrate --ref-format=reftable

From /tmp/src
 * [new branch]      Dup           -> origin/Dup
 ! [new branch]      dup           -> origin/dup  (unable to update local ref)
 * [new branch]      main          -> origin/main
`

// fakeCaseConflictFetch makes every cache fetch run for real and then report
// a ref case conflict, as it does on a case-insensitive filesystem. It swaps
// a package variable, so callers must not be parallel tests.
func fakeCaseConflictFetch(t *testing.T) {
	t.Helper()
	orig := runGitFetchOutput
	runGitFetchOutput = func(ctx context.Context, barePath string) ([]byte, error) {
		out, err := runGitFetchOutputContext(ctx, barePath)
		if err != nil {
			return out, err
		}
		return append(out, caseConflictFetchOutput...), errors.New("exit status 1")
	}
	t.Cleanup(func() { runGitFetchOutput = orig })
}

func TestClassifyFetchErrorRecognizesCaseConflict(t *testing.T) {
	t.Parallel()
	err := classifyFetchError([]byte(caseConflictFetchOutput), errors.New("exit status 1"))
	var conflict *RefCaseConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected RefCaseConflictError, got %v", err)
	}
	if want := []string{"origin/dup"}; !slices.Equal(conflict.Refs, want) {
		t.Fatalf("refs = %q, want %q", conflict.Refs, want)
	}
	if !strings.Contains(err.Error(), "origin/dup") || !strings.Contains(err.Error(), "still usable") {
		t.Fatalf("message must name the ref and say the cache is usable: %v", err)
	}

	other := classifyFetchError([]byte("fatal: unable to access 'x'\n"), errors.New("exit status 128"))
	if IsRefCaseConflict(other) {
		t.Fatalf("an ordinary failure was classified as a case conflict: %v", other)
	}
	if got, want := other.Error(), "git fetch: fatal: unable to access 'x': exit status 128"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// The reported failure: the clone downloads everything, then the fetch that
// converts it to the remote-tracking layout reports a case conflict, and the
// whole cache used to be deleted. It must be kept and usable instead, and a
// later fetch on the cache must report the conflict as a warning.
func TestCaseConflictDuringCloneKeepsCache(t *testing.T) {
	fakeCaseConflictFetch(t)
	source := createTestRepo(t)
	cache := New(t.TempDir(), testLogger())

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync of a cold cache failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", source)
	if barePath == "" {
		t.Fatal("cache was not kept after a ref case conflict")
	}
	if refspec, err := readFetchRefspec(barePath); err != nil || refspec != modernFetchRefspec {
		t.Fatalf("refspec = %q (%v), want %q", refspec, err, modernFetchRefspec)
	}
	if getRemoteDefaultBranch(barePath) == "" {
		t.Fatal("cache has no default branch after the conflict")
	}

	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     source,
		WorkDir:     t.TempDir(),
		AgentName:   "tester",
		TaskID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
	if !strings.Contains(result.FetchError, "letter case") {
		t.Fatalf("checkout should carry the case-conflict warning, got %q", result.FetchError)
	}

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); !IsRefCaseConflict(err) {
		t.Fatalf("Sync of a cached repo should report the conflict, got %v", err)
	}
	if cache.Lookup("ws-1", source) == "" {
		t.Fatal("a fetch warning must not remove the cache")
	}
}

// The same scenario on a real case-insensitive filesystem (macOS CI runs it
// through `-run CaseConflict`). Skipped where the filesystem is case-sensitive.
func TestCaseConflictOnCaseInsensitiveFilesystem(t *testing.T) {
	t.Parallel()
	probeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(probeDir, "probe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(probeDir, "PROBE")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	if out, err := exec.Command("git", "--version").Output(); err == nil {
		t.Logf("using %s", strings.TrimSpace(string(out)))
	}

	source := createTestRepo(t)
	head, err := runGitOutput("-C", source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if err := runGit("-C", source, "pack-refs", "--all"); err != nil {
		t.Fatalf("pack-refs: %v", err)
	}
	// Loose refs cannot hold both names on this filesystem, so write them
	// into packed-refs directly. Without the "sorted" trait Git sorts the
	// file on read.
	packed, err := os.ReadFile(filepath.Join(source, ".git", "packed-refs"))
	if err != nil {
		t.Skipf("source does not use the files ref backend: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(packed)), "\n") {
		if strings.HasPrefix(line, "#") {
			line = "# pack-refs with: peeled fully-peeled "
		}
		lines = append(lines, line)
	}
	sha := strings.TrimSpace(string(head))
	lines = append(lines, sha+" refs/heads/feature/Dup", sha+" refs/heads/feature/dup")
	if err := os.WriteFile(filepath.Join(source, ".git", "packed-refs"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write packed-refs: %v", err)
	}
	refs, err := runGitOutput("-C", source, "for-each-ref", "--format=%(refname)", "refs/heads/feature")
	if err != nil || strings.Count(string(refs), "\n") != 2 {
		t.Skipf("could not create refs that differ only in case: %q %v", refs, err)
	}

	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync of a cold cache failed: %v", err)
	}
	if cache.Lookup("ws-1", source) == "" {
		t.Fatal("cache was not kept after a ref case conflict")
	}
	if _, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     source,
		WorkDir:     t.TempDir(),
		AgentName:   "tester",
		TaskID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	}); err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
}

// A slow clone is modeled by holding the repository lock, so the build cannot
// finish while the callers wait. Callers that stop waiting get a retryable
// error, share one build, and the build completes after they are gone.
func TestBuildOutlivesCallersThatStopWaiting(t *testing.T) {
	t.Parallel()
	source := createTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	lock := cache.lockForRepo(filepath.Join(cache.root, "ws-1", bareDirName(source)))
	lock.Lock()

	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		err := cache.BuildContext(ctx, "ws-1", source)
		cancel()
		if !errors.Is(err, ErrCacheBuilding) || !errors.Is(err, context.DeadlineExceeded) {
			lock.Unlock()
			t.Fatalf("expected a pending build, got %v", err)
		}
		if !strings.Contains(err.Error(), "retry `multica repo checkout`") {
			lock.Unlock()
			t.Fatalf("pending error does not say how to proceed: %v", err)
		}
	}
	cache.builds.mu.Lock()
	jobs := len(cache.builds.jobs)
	cache.builds.mu.Unlock()
	if jobs != 1 {
		lock.Unlock()
		t.Fatalf("callers started %d builds, want 1", jobs)
	}
	if cache.Lookup("ws-1", source) != "" {
		lock.Unlock()
		t.Fatal("cache is visible before its build finished")
	}

	lock.Unlock()
	if err := cache.BuildContext(t.Context(), "ws-1", source); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if cache.Lookup("ws-1", source) == "" {
		t.Fatal("cache missing after the build finished")
	}
}

func TestFailedBuildLeavesNoCacheOrStagingClone(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing-repo")
	cache := New(t.TempDir(), testLogger())

	err := cache.Sync("ws-1", []RepoInfo{{URL: missing}})
	var cloneErr *CloneError
	if !errors.As(err, &cloneErr) {
		t.Fatalf("expected a CloneError, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "git clone --bare: ") {
		t.Fatalf("error = %q, want git's clone output", err)
	}
	if cache.Lookup("ws-1", missing) != "" {
		t.Fatal("a failed build left a cache behind")
	}
	if _, err := os.Stat(cache.BarePath("ws-1", missing)); !os.IsNotExist(err) {
		t.Fatalf("a failed build left the cache path behind: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(cache.root, stagingDirName, "ws-1"))
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed build left staging clones behind: %v", entries)
	}
}

func TestBuildRemovesOnlyItsOwnStaleStagingClones(t *testing.T) {
	t.Parallel()
	source := createTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	stageDir := filepath.Join(cache.root, stagingDirName, "ws-1")
	name := bareDirName(source)
	stale := filepath.Join(stageDir, name+"-123")
	otherRepo := filepath.Join(stageDir, "other-0123456789ab.git-456")
	notStaging := filepath.Join(stageDir, name+"-keep")
	for _, dir := range []string{stale, otherRepo, notStaging} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stale, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging clone of this repo was kept: %v", err)
	}
	for _, dir := range []string{otherRepo, notStaging} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(dir), err)
		}
	}
}

func TestIsStagingDirOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		entry string
		want  bool
	}{
		{"repo-0123.git-42", true},
		{"repo-0123.git-", false},
		{"repo-0123.git", false},
		{"repo-0123.git-4x2", false},
		{"other.git-42", false},
		{"repo-0123.git-42-1", false},
	} {
		if got := isStagingDirOf(tc.entry, "repo-0123.git"); got != tc.want {
			t.Errorf("isStagingDirOf(%q) = %v, want %v", tc.entry, got, tc.want)
		}
	}
}

func TestBloblessEnabledMatchesRepositoryIdentity(t *testing.T) {
	t.Setenv(BloblessEnv, " https://github.com/org/big.git , git@github.com:org/other ")
	for url, want := range map[string]bool{
		"git@github.com:org/big":         true,
		"https://github.com/org/big":     true,
		"https://github.com/org/other":   true,
		"https://github.com/org/small":   false,
		"https://gitlab.example.com/big": false,
	} {
		if got := bloblessEnabled(url); got != want {
			t.Errorf("bloblessEnabled(%q) = %v, want %v", url, got, want)
		}
	}
	t.Setenv(BloblessEnv, "*")
	if !bloblessEnabled("https://github.com/org/small") {
		t.Error(`"*" must select every repository`)
	}
	t.Setenv(BloblessEnv, "")
	if bloblessEnabled("https://github.com/org/big") {
		t.Error("unset must keep full clones")
	}
}

func TestCloneTimeoutFromEnv(t *testing.T) {
	t.Setenv(CloneTimeoutEnv, "")
	if got := cloneTimeout(); got != defaultCloneTimeout {
		t.Fatalf("default = %s, want %s", got, defaultCloneTimeout)
	}
	t.Setenv(CloneTimeoutEnv, "3h")
	if got := cloneTimeout(); got != 3*time.Hour {
		t.Fatalf("override = %s, want 3h", got)
	}
	t.Setenv(CloneTimeoutEnv, "soon")
	if got := cloneTimeout(); got != defaultCloneTimeout {
		t.Fatalf("invalid value = %s, want the default", got)
	}
}

// A listed repository gets a blobless cache that already holds the default
// branch's files, so checkouts neither miss nor re-download them.
func TestBloblessBuildPrefetchesDefaultBranch(t *testing.T) {
	source := createFilterableTestRepo(t)
	t.Setenv(BloblessEnv, source)
	cache := New(t.TempDir(), testLogger())

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", source)
	if barePath == "" || !isPartialClone(barePath) {
		t.Fatalf("expected a blobless cache at %q", barePath)
	}
	ref := getRemoteDefaultBranch(barePath)
	out, err := runGitOutput("-C", barePath, "rev-list", "--objects", "--missing=print", "--no-walk", ref)
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	if strings.Contains(string(out), "?") {
		t.Fatalf("default branch files are missing from the cache:\n%s", out)
	}

	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     source,
		WorkDir:     t.TempDir(),
		AgentName:   "tester",
		TaskID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
	assertCheckoutIsComplete(t, result.Path)
}

// Existing full caches, and repositories that are not listed, keep full
// clones when the switch is turned on.
func TestFullCachesStayFullWhenBloblessIsEnabled(t *testing.T) {
	existing := createFilterableTestRepo(t)
	unlisted := createFilterableTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: existing}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	t.Setenv(BloblessEnv, "*")
	if err := cache.Sync("ws-1", []RepoInfo{{URL: existing}}); err != nil {
		t.Fatalf("fetch of the existing cache failed: %v", err)
	}
	if isPartialClone(cache.Lookup("ws-1", existing)) {
		t.Fatal("an existing full cache became a partial clone")
	}

	t.Setenv(BloblessEnv, "https://example.com/some/other.git")
	if err := cache.Sync("ws-1", []RepoInfo{{URL: unlisted}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if isPartialClone(cache.Lookup("ws-1", unlisted)) {
		t.Fatal("a repository that is not listed got a blobless cache")
	}
}

func TestCloneFailureHint(t *testing.T) {
	t.Parallel()
	const url = "https://example.com/org/big.git"
	for _, tc := range []struct {
		output string
		err    error
		want   string
	}{
		{"fetch-pack: unexpected disconnect while reading sideband packet", errors.New("git command timed out after 10m0s: context deadline exceeded"), BloblessEnv + "=" + url},
		{"error: RPC failed; curl 56 Recv failure: Connection reset by peer", errors.New("exit status 128"), CloneTimeoutEnv},
		{"fatal: could not read Username for 'https://example.com': terminal prompts disabled", errors.New("exit status 128"), "credentials"},
		{"fatal: repository '/nowhere' does not exist", errors.New("exit status 128"), ""},
	} {
		got := cloneFailureHint(tc.output, tc.err, url)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("hint for %q = %q, want it to mention %q", tc.output, got, tc.want)
		}
	}
}
