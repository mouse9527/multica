// Added by the mouse9527/multica fork: resilient repo cache rebuilds (M-161).

package repocache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	err := classifyFetchError([]byte(caseConflictFetchOutput), errors.New("exit status 1"), nil)
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

	other := classifyFetchError([]byte("fatal: unable to access 'x'\n"), errors.New("exit status 128"), nil)
	if IsRefCaseConflict(other) {
		t.Fatalf("an ordinary failure was classified as a case conflict: %v", other)
	}
	if got, want := other.Error(), "git fetch: fatal: unable to access 'x': exit status 128"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// unexplainedCaseConflictOutput is a refused ref without Git's explanation of
// the case conflict, as from a Git version that words it differently.
const unexplainedCaseConflictOutput = `From /tmp/src
 * [new branch]      Dup           -> origin/Dup
 ! [new branch]      dup           -> origin/dup  (unable to update local ref)
 * [new branch]      main          -> origin/main
`

func TestClassifyFetchErrorRecognizesUnexplainedCaseConflict(t *testing.T) {
	t.Parallel()
	exit1 := errors.New("exit status 1")
	stored := func(refs ...string) func() []string { return func() []string { return refs } }

	err := classifyFetchError([]byte(unexplainedCaseConflictOutput), exit1, stored("origin/Dup", "origin/main"))
	var conflict *RefCaseConflictError
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Refs, []string{"origin/dup"}) {
		t.Fatalf("expected a conflict over origin/dup, got %v", err)
	}

	for name, storedRefs := range map[string]func() []string{
		"no ref differs only in case": stored("origin/main"),
		"the refused ref itself":      stored("origin/dup", "origin/main"),
		"refs unknown":                nil,
	} {
		if err := classifyFetchError([]byte(unexplainedCaseConflictOutput), exit1, storedRefs); IsRefCaseConflict(err) {
			t.Errorf("%s: a refused ref was excused: %v", name, err)
		}
	}
	lockFailure := " ! [new branch]      dup           -> origin/dup  (unable to update local ref)\n" +
		" ! [new branch]      other         -> origin/other  (unable to update local ref)\n"
	if err := classifyFetchError([]byte(lockFailure), exit1, stored("origin/Dup")); IsRefCaseConflict(err) {
		t.Errorf("a ref refused for another reason was excused: %v", err)
	}
}

// Without Git's explanation, a fetch that refused only a ref differing in case
// from one the cache holds still keeps the cache and reports a warning.
func TestUnexplainedCaseConflictKeepsCache(t *testing.T) {
	source := createTestRepo(t)
	if err := runGit("-C", source, "branch", "feature/Dup"); err != nil {
		t.Fatalf("branch: %v", err)
	}
	orig := runGitFetchOutput
	runGitFetchOutput = func(ctx context.Context, barePath string) ([]byte, error) {
		out, err := runGitFetchOutputContext(ctx, barePath)
		if err != nil {
			return out, err
		}
		refused := " ! [new branch]      feature/dup   -> origin/feature/dup  (unable to update local ref)\n"
		return append(out, refused...), errors.New("exit status 1")
	}
	t.Cleanup(func() { runGitFetchOutput = orig })
	cache := New(t.TempDir(), testLogger())

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync of a cold cache failed: %v", err)
	}
	if cache.Lookup("ws-1", source) == "" {
		t.Fatal("cache was not kept")
	}
	err := cache.Sync("ws-1", []RepoInfo{{URL: source}})
	var conflict *RefCaseConflictError
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Refs, []string{"origin/feature/dup"}) {
		t.Fatalf("Sync of a cached repo should report the conflict over origin/feature/dup, got %v", err)
	}
}

// failCacheFetches makes the first n cache fetches fail as a dropped link
// does. It swaps a package variable, so callers must not be parallel tests.
func failCacheFetches(t *testing.T, n int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	orig := runGitFetchOutput
	runGitFetchOutput = func(ctx context.Context, barePath string) ([]byte, error) {
		mu.Lock()
		calls++
		fail := calls <= n
		mu.Unlock()
		if fail {
			return []byte("error: RPC failed; curl 56 Recv failure: Connection reset by peer\nfatal: early EOF\n"), errors.New("exit status 128")
		}
		return runGitFetchOutputContext(ctx, barePath)
	}
	t.Cleanup(func() { runGitFetchOutput = orig })
}

// A complete clone is kept when the fetch that fills refs/remotes/origin/*
// fails: it is retried once, and if that fails too the cache still checks
// out the default branch from the clone's own refs.
func TestFetchFailureAfterCloneKeepsCache(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failures      int
		wantOriginRef bool
	}{
		{"retry succeeds", 1, true},
		{"retry fails", 2, false},
		{"checkout fetch fails too", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failCacheFetches(t, tc.failures)
			source := createTestRepo(t)
			head, err := runGitOutput("-C", source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatalf("rev-parse: %v", err)
			}
			cache := New(t.TempDir(), testLogger())

			if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
				t.Fatalf("Sync of a cold cache failed: %v", err)
			}
			barePath := cache.Lookup("ws-1", source)
			if barePath == "" {
				t.Fatal("the clone was discarded")
			}
			if refspec, err := readFetchRefspec(barePath); err != nil || refspec != modernFetchRefspec {
				t.Fatalf("refspec = %q (%v), want %q", refspec, err, modernFetchRefspec)
			}
			out, err := runGitOutput("-C", barePath, "for-each-ref", "refs/remotes/origin")
			if err != nil {
				t.Fatalf("for-each-ref: %v", err)
			}
			if got := strings.TrimSpace(string(out)) != ""; got != tc.wantOriginRef {
				t.Fatalf("refs/remotes/origin/* filled = %v, want %v", got, tc.wantOriginRef)
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
			if got, err := resolveCommitContext(t.Context(), result.Path, "HEAD"); err != nil || got != strings.TrimSpace(string(head)) {
				t.Fatalf("checkout HEAD = %s (%v), want %s", got, err, head)
			}
		})
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

// A listed repository gets a blobless cache that is published as soon as its
// clone is done, before the default branch's files arrive. They follow in a
// cache-owned download, which a checkout joins instead of starting another.
func TestBloblessBuildPublishesBeforeFilesArrive(t *testing.T) {
	slow := installSlowBlobFetch(t)
	source := createFilterableTestRepo(t)
	t.Setenv(BloblessEnv, source)
	cache := New(t.TempDir(), testLogger())
	var once sync.Once
	gate := slow.closeGateLater()
	release := func() { once.Do(gate) }
	t.Cleanup(release)

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", source)
	if barePath == "" || !isPartialClone(barePath) {
		t.Fatalf("expected a blobless cache at %q", barePath)
	}
	commit, err := resolveCommitContext(t.Context(), barePath, getRemoteDefaultBranch(barePath))
	if err != nil {
		t.Fatalf("resolve default branch: %v", err)
	}
	if missing, err := missingObjectsContext(t.Context(), barePath, commit); err != nil || len(missing) == 0 {
		t.Fatalf("the build waited for the default branch's files (missing %v, %v)", missing, err)
	}

	release()
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
	if calls, _ := slow.stats(); calls != 1 {
		t.Fatalf("file downloads = %d, want the build's one", calls)
	}
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

// slowBlobFetch makes file downloads wait for the gate current at the time
// they start, and records the time limit they were given. It swaps package
// variables, so callers must not be parallel tests.
type slowBlobFetch struct {
	mu       sync.Mutex
	gate     chan struct{}
	calls    int
	timeouts []time.Duration
}

func installSlowBlobFetch(t *testing.T) *slowBlobFetch {
	t.Helper()
	slow := &slowBlobFetch{gate: make(chan struct{})}
	close(slow.gate)
	orig := runBlobFetch
	runBlobFetch = func(ctx context.Context, barePath string, oids []string, timeout time.Duration) ([]byte, error) {
		slow.mu.Lock()
		gate := slow.gate
		slow.calls++
		slow.timeouts = append(slow.timeouts, timeout)
		slow.mu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return runBlobFetchContext(ctx, barePath, oids, timeout)
	}
	t.Cleanup(func() { runBlobFetch = orig })
	return slow
}

func (s *slowBlobFetch) closeGateLater() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	return func() { close(gate) }
}

func (s *slowBlobFetch) stats() (int, []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, slices.Clone(s.timeouts)
}

// The file download of a blobless build gets the build's limit, not the
// fixed per-command one.
func TestBloblessBuildDownloadsFilesWithinTheBuildLimit(t *testing.T) {
	slow := installSlowBlobFetch(t)
	source := createFilterableTestRepo(t)
	t.Setenv(BloblessEnv, source)
	t.Setenv(CloneTimeoutEnv, "3h")
	cache := New(t.TempDir(), testLogger())

	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	// The download runs after the build; a checkout waits for it.
	if _, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     source,
		WorkDir:     t.TempDir(),
		AgentName:   "tester",
		TaskID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	}); err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
	calls, timeouts := slow.stats()
	if calls != 1 || timeouts[0] != 3*time.Hour {
		t.Fatalf("file download calls = %d with limits %v, want one with 3h", calls, timeouts)
	}
}

// On a slow link a checkout of a blobless cache stops waiting for the files
// it needs, the download carries on without any request, and the retry checks
// out. Covers an old commit and a branch other than the default.
func TestSlowFileDownloadOutlivesTheCheckoutRequest(t *testing.T) {
	slow := installSlowBlobFetch(t)
	origBudget := checkoutBlobWaitBudget
	checkoutBlobWaitBudget = 50 * time.Millisecond
	t.Cleanup(func() { checkoutBlobWaitBudget = origBudget })

	source := createFilterableTestRepo(t)
	dir := strings.TrimPrefix(source, "file://")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	// The old commit holds only a.txt; the feature branch adds feature.txt.
	// Check out the old commit first: its files are a subset of the branch's.
	oldCommit := git("-C", dir, "rev-parse", "HEAD~1")
	git("-C", dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("-C", dir, "add", "feature.txt")
	git("-C", dir, "commit", "-q", "-m", "feature")
	git("-C", dir, "checkout", "-q", "-")

	cache := New(t.TempDir(), testLogger())
	barePath := seedBloblessCache(t, cache, "ws-1", source)

	for i, ref := range []string{oldCommit, "feature"} {
		params := WorktreeParams{
			WorkspaceID: "ws-1",
			RepoURL:     source,
			WorkDir:     t.TempDir(),
			Ref:         ref,
			AgentName:   "tester",
			TaskID:      fmt.Sprintf("a1b2c3d4-e5f6-7890-abcd-ef123456789%d", i),
		}
		callsBefore, _ := slow.stats()
		release := slow.closeGateLater()
		for range 2 {
			_, err := cache.CreateWorktree(params)
			if !errors.Is(err, ErrCacheBuilding) || !strings.Contains(err.Error(), "still being downloaded") {
				release()
				t.Fatalf("%s: expected a retryable pending download, got %v", ref, err)
			}
		}
		if calls, _ := slow.stats(); calls != callsBefore+1 {
			release()
			t.Fatalf("%s: requests started %d downloads, want 1", ref, calls-callsBefore)
		}

		// No request is waiting now; the download must still finish.
		release()
		commit := git("-C", dir, "rev-parse", ref)
		deadline := time.Now().Add(30 * time.Second)
		for {
			missing, err := missingObjectsContext(context.Background(), barePath, commit)
			if err == nil && len(missing) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: download did not finish after the requests ended: %v %v", ref, missing, err)
			}
			time.Sleep(20 * time.Millisecond)
		}

		result, err := cache.CreateWorktree(params)
		if err != nil {
			t.Fatalf("%s: retry failed: %v", ref, err)
		}
		if status := git("-C", result.Path, "status", "--porcelain"); status != "" {
			t.Fatalf("%s: checkout is missing files:\n%s", ref, status)
		}
		if got, err := resolveCommitContext(context.Background(), result.Path, "HEAD"); err != nil || got != commit {
			t.Fatalf("%s: checkout HEAD = %s (%v), want %s", ref, got, err, commit)
		}
	}
	if _, timeouts := slow.stats(); !slices.ContainsFunc(timeouts, func(d time.Duration) bool { return d == cloneTimeout() }) {
		t.Fatalf("checkout downloads ran with limits %v, want the build limit %s", timeouts, cloneTimeout())
	}
}

func TestWaitContextTakesTheEarlierDeadline(t *testing.T) {
	t.Parallel()
	soon := time.Now().Add(time.Minute)
	for name, tc := range map[string]struct {
		ctx    context.Context
		budget time.Duration
		want   time.Time
	}{
		"no shared deadline":          {context.Background(), time.Hour, time.Now().Add(time.Hour)},
		"shared deadline comes first": {WithWaitDeadline(context.Background(), soon), time.Hour, soon},
		"budget comes first":          {WithWaitDeadline(context.Background(), soon), time.Second, time.Now().Add(time.Second)},
	} {
		ctx, cancel := WaitContext(tc.ctx, tc.budget)
		deadline, ok := ctx.Deadline()
		cancel()
		if !ok || deadline.Sub(tc.want).Abs() > 5*time.Second {
			t.Errorf("%s: deadline = %v (%v), want about %v", name, deadline, ok, tc.want)
		}
	}
}

// A checkout that already spent its request's wait on a cache build does not
// wait the full file download budget on top of it.
func TestCheckoutFileWaitSharesTheRequestDeadline(t *testing.T) {
	slow := installSlowBlobFetch(t)
	source := createFilterableTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	seedBloblessCache(t, cache, "ws-1", source)
	release := slow.closeGateLater()

	ctx := WithWaitDeadline(t.Context(), time.Now().Add(50*time.Millisecond))
	start := time.Now()
	_, err := cache.CreateWorktreeContext(ctx, WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     source,
		WorkDir:     t.TempDir(),
		AgentName:   "tester",
		TaskID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	})
	elapsed := time.Since(start)
	release()
	waitForBlobJobs(t, cache)
	var pending *BlobsPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("expected a pending file download, got %v", err)
	}
	if elapsed > checkoutBlobWaitBudget/2 {
		t.Fatalf("checkout waited %s, past the request's deadline", elapsed)
	}
}

// waitForBlobJobs waits for the cache's file downloads to end, so none
// outlives the test's temporary directories.
func waitForBlobJobs(t *testing.T, cache *Cache) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		cache.builds.mu.Lock()
		running := len(cache.builds.blobJobs)
		cache.builds.mu.Unlock()
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d file downloads still running", running)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Once a commit's files are all in the cache, later checkouts of it skip
// listing its tree again.
func TestCheckoutListsAFullCommitOnce(t *testing.T) {
	source := createFilterableTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	barePath := seedBloblessCache(t, cache, "ws-1", source)
	var mu sync.Mutex
	listed := 0
	orig := listMissingObjects
	listMissingObjects = func(ctx context.Context, barePath, commit string) ([]string, error) {
		mu.Lock()
		listed++
		mu.Unlock()
		return missingObjectsContext(ctx, barePath, commit)
	}
	t.Cleanup(func() { listMissingObjects = orig })
	listings := func() int {
		mu.Lock()
		defer mu.Unlock()
		return listed
	}
	checkout := func(i int) {
		t.Helper()
		result, err := cache.CreateWorktree(WorktreeParams{
			WorkspaceID: "ws-1",
			RepoURL:     source,
			WorkDir:     t.TempDir(),
			AgentName:   "tester",
			TaskID:      fmt.Sprintf("a1b2c3d4-e5f6-7890-abcd-ef123456789%d", i),
		})
		if err != nil {
			t.Fatalf("CreateWorktree %d failed: %v", i, err)
		}
		assertCheckoutIsComplete(t, result.Path)
	}

	checkout(0)
	first := listings()
	if first == 0 {
		t.Fatal("the first checkout did not look for missing files")
	}
	checkout(1)
	checkout(2)
	if more := listings() - first; more != 0 {
		t.Fatalf("later checkouts listed the tree %d more times, want 0", more)
	}

	cache.forgetCompleteBlobs(barePath)
	if commit, err := resolveCommitContext(t.Context(), barePath, getRemoteDefaultBranch(barePath)); err != nil || cache.hasCompleteBlobs(barePath, commit) {
		t.Fatalf("a rebuilt cache must not inherit what was known about the old one (%v)", err)
	}
}
