// Added by the mouse9527/multica fork: resilient repo cache rebuilds (M-161).
//
// A cache rebuild used to run inside whatever asked for it (usually a
// `multica repo checkout` request with a 5-minute client deadline), under a
// fixed 10-minute per-command ceiling, and it deleted the whole download when
// the post-clone fetch reported refs that differ only in letter case. On a
// case-insensitive filesystem with a slow link to the Git host a large
// repository could therefore never be cached. This file holds the fork's
// replacement:
//
//   - builds run in a job owned by the Cache, so a caller giving up stops
//     waiting but does not kill the clone;
//   - the clone is staged outside the cache directory and renamed into place
//     only when complete, so a half-built cache is never visible and a
//     failure removes only its own staging directory;
//   - the clone is bounded by a stall check plus a generous total limit
//     instead of a fixed 10 minutes;
//   - repositories listed in MULTICA_REPO_CACHE_BLOBLESS are cloned without
//     file contents and published as soon as that clone is done; the files a
//     checkout needs, starting with the default branch's, are fetched into
//     the cache by a cache-owned job, so a slow download outlives the
//     checkout request and the retry finds them;
//   - a ref case conflict is a warning, not a failure, and so is a failed
//     fetch right after a complete clone;
//   - the waits of one checkout request share one budget (WithWaitDeadline).
package repocache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/processtree"
)

const (
	// BloblessEnv lists the repositories whose new caches are blobless
	// partial clones: comma-separated repository URLs (any spelling of the
	// same repository matches), or "*" for every repository. Unset keeps
	// upstream behavior: full clones.
	BloblessEnv = "MULTICA_REPO_CACHE_BLOBLESS"
	// CloneTimeoutEnv overrides the total time a cache build may take
	// (a Go duration such as "3h").
	CloneTimeoutEnv = "MULTICA_REPO_CACHE_CLONE_TIMEOUT"

	defaultCloneTimeout = 2 * time.Hour

	// A transfer slower than cloneLowSpeedLimit bytes/s for cloneLowSpeedTime
	// seconds is a stalled link, not a slow one; Git aborts it so the build
	// fails with a clear reason instead of waiting out the total limit.
	cloneLowSpeedLimit = "1000"
	cloneLowSpeedTime  = "300"

	// stagingDirName holds in-progress clones, two levels below the cache
	// root (<root>/.partial/<workspace>/<name>-<n>). GC only inspects
	// <root>/<workspace>/<entry>, so it never mistakes a staging clone for a
	// cache. The (empty) directories are kept: removing them could race a
	// concurrent build of another repository creating its staging clone.
	stagingDirName = ".partial"
)

// ErrCacheBuilding means a cache build is still running after the caller
// stopped waiting for it. The build continues; retrying later finds the
// cache or joins the same build.
var ErrCacheBuilding = errors.New("repository cache is still being built")

// BuildPendingError reports a build the caller stopped waiting for. It
// matches ErrCacheBuilding and unwraps to the reason the wait ended.
type BuildPendingError struct {
	URL     string
	Running time.Duration
	Cause   error
}

func (e *BuildPendingError) Error() string {
	return fmt.Sprintf("repository cache for %s is still being built in the background (running for %s); retry `multica repo checkout` in a few minutes",
		e.URL, e.Running.Round(time.Second))
}

func (e *BuildPendingError) Is(target error) bool { return target == ErrCacheBuilding }

func (e *BuildPendingError) Unwrap() error { return e.Cause }

// RefCaseConflictError means `git fetch` stored every ref except the ones
// that differ from another ref only in letter case, which a case-insensitive
// filesystem cannot hold side by side. The cache stays usable.
type RefCaseConflictError struct {
	Refs []string
}

func (e *RefCaseConflictError) Error() string {
	refs := "some refs"
	if len(e.Refs) > 0 {
		refs = strings.Join(e.Refs, ", ")
	}
	return fmt.Sprintf("git fetch stored every ref except %s: they differ from another ref only in letter case, which this case-insensitive filesystem cannot store side by side. The cache is still usable; to clear this warning, delete or rename one branch of each such pair on the remote", refs)
}

// IsRefCaseConflict reports whether err is (or wraps) a RefCaseConflictError.
func IsRefCaseConflict(err error) bool {
	var conflict *RefCaseConflictError
	return errors.As(err, &conflict)
}

// CloneError is a failed `git clone`, with a hint at the likely fix.
type CloneError struct {
	Output string
	Err    error
	Hint   string
}

func (e *CloneError) Error() string {
	msg := fmt.Sprintf("git clone --bare: %s: %v", e.Output, e.Err)
	if e.Hint != "" {
		msg += ". " + e.Hint
	}
	return msg
}

func (e *CloneError) Unwrap() error { return e.Err }

// buildRegistry tracks the cache builds in flight, at most one per
// repository and workspace, the file downloads, at most one per cache and
// commit, and the (cache, commit) pairs known to have all their files.
type buildRegistry struct {
	mu       sync.Mutex
	jobs     map[string]*buildJob
	blobJobs map[string]*buildJob

	// blobsComplete spares a checkout from listing a large tree's missing
	// files again. A stale entry, possible only if objects vanish from the
	// cache by other means, costs the checkout a lazy fetch, as without it.
	blobsComplete map[string]struct{}
}

type buildJob struct {
	done    chan struct{}
	err     error
	started time.Time
}

// BuildContext makes sure the cache for repoURL exists, starting a build or
// joining the one in flight. The build belongs to the Cache: when ctx ends
// first, BuildContext returns a BuildPendingError and the build carries on.
func (c *Cache) BuildContext(ctx context.Context, workspaceID, repoURL string) error {
	job := c.startBuild(workspaceID, repoURL)
	select {
	case <-job.done:
		return job.err
	case <-ctx.Done():
		return &BuildPendingError{URL: repoURL, Running: time.Since(job.started), Cause: context.Cause(ctx)}
	}
}

func (c *Cache) startBuild(workspaceID, repoURL string) *buildJob {
	key := workspaceID + "\x00" + legacyBareDirName(repoURL)
	c.builds.mu.Lock()
	defer c.builds.mu.Unlock()
	if job, ok := c.builds.jobs[key]; ok {
		return job
	}
	if c.builds.jobs == nil {
		c.builds.jobs = make(map[string]*buildJob)
	}
	job := &buildJob{done: make(chan struct{}), started: time.Now()}
	c.builds.jobs[key] = job
	go func() {
		err := c.runBuild(workspaceID, repoURL)
		c.builds.mu.Lock()
		delete(c.builds.jobs, key)
		c.builds.mu.Unlock()
		job.err = err
		close(job.done)
	}()
	return job
}

func (c *Cache) runBuild(workspaceID, repoURL string) error {
	timeout := cloneTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	unlock, err := c.lockRepoURLContext(ctx, workspaceID, repoURL)
	if err != nil {
		return err
	}
	defer unlock()
	barePath, err := c.prepareBarePathContext(ctx, workspaceID, repoURL)
	if err != nil {
		return err
	}
	if isBareRepo(barePath) {
		return nil
	}

	blobless := bloblessEnabled(repoURL)
	c.logger.Info("repo cache: cloning", "url", repoURL, "path", barePath, "blobless", blobless)
	stage, err := c.newStagingDir(workspaceID, filepath.Base(barePath))
	if err != nil {
		return err
	}
	opts := cloneOptions{
		timeout:  timeout,
		blobless: blobless,
		warn: func(err error) {
			c.logger.Warn("repo cache: clone kept despite a fetch warning", "url", repoURL, "error", err)
		},
	}
	if err := cloneBareContext(ctx, repoURL, stage, opts); err != nil {
		c.logger.Error("repo cache: clone failed", "url", repoURL, "error", err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		_ = os.RemoveAll(stage)
		return fmt.Errorf("create workspace cache dir: %w", err)
	}
	// What was known about an earlier cache at this path (evicted since)
	// does not hold for the new clone.
	c.forgetCompleteBlobs(barePath)
	if err := os.Rename(stage, barePath); err != nil {
		_ = os.RemoveAll(stage)
		err = fmt.Errorf("move finished clone into the cache at %s: %w. If that path holds a leftover directory that is not a Git repository, remove it by hand and retry", barePath, err)
		c.logger.Error("repo cache: clone failed", "url", repoURL, "error", err)
		return err
	}
	c.logger.Info("repo cache: cloned", "url", repoURL, "path", barePath)
	if blobless {
		// The cache is usable before the default branch's files arrive: a
		// checkout waits for (or joins) this download, not the whole build.
		c.prefetchDefaultBranchInBackground(ctx, barePath)
	}
	return nil
}

// newStagingDir creates an empty directory for a new clone of the cache
// named name, after removing that cache's leftover staging directories. The
// caller holds the repository lock, so no other build of this cache is using
// them; a Git process orphaned by an earlier daemon writes into its own
// uniquely named directory and only loses its work.
func (c *Cache) newStagingDir(workspaceID, name string) (string, error) {
	dir := filepath.Join(c.root, stagingDirName, workspaceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create clone staging dir: %w", err)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if isStagingDirOf(entry.Name(), name) {
				_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
			}
		}
	}
	stage, err := os.MkdirTemp(dir, name+"-")
	if err != nil {
		return "", fmt.Errorf("create clone staging dir: %w", err)
	}
	return stage, nil
}

// isStagingDirOf matches the names os.MkdirTemp(dir, name+"-") produces:
// name, a dash, and a run of digits.
func isStagingDirOf(entry, name string) bool {
	suffix, ok := strings.CutPrefix(entry, name+"-")
	if !ok || suffix == "" {
		return false
	}
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

type cloneOptions struct {
	timeout  time.Duration
	blobless bool
	// warn receives problems that do not fail the clone, such as a ref case
	// conflict. Nil drops them.
	warn func(error)
}

// cloneBareContext clones url into dest (which must be absent or empty) and
// converts it to the remote-tracking layout. A blobless clone gets no file
// contents here; the caller fetches them. On failure it removes dest.
func cloneBareContext(ctx context.Context, url, dest string, opts cloneOptions) (retErr error) {
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(dest)
		}
	}()
	warn := opts.warn
	if warn == nil {
		warn = func(error) {}
	}
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = repoCacheGitTimeout
	}

	args := append(caCloneArgs(),
		"-c", "http.lowSpeedLimit="+cloneLowSpeedLimit,
		"-c", "http.lowSpeedTime="+cloneLowSpeedTime,
		"clone", "--bare")
	if opts.blobless {
		args = append(args, "--filter="+partialCloneFilter)
	}
	args = append(args, url, dest)
	if out, err := runGitCombinedOutputWithTimeoutContext(ctx, timeout, args...); err != nil {
		output := strings.TrimSpace(string(out))
		return &CloneError{Output: output, Err: err, Hint: cloneFailureHint(output, err, url)}
	}
	// Record the CA bundle before the refspec migration below fetches again.
	if err := configureRepoCABundleContext(ctx, dest); err != nil {
		return err
	}
	// `git clone --bare` populates refs/heads/* as a snapshot and defaults to
	// a mirror-style fetch refspec. Convert the bare repo to the standard
	// remote-tracking layout immediately so subsequent fetches write to
	// refs/remotes/origin/* and can't conflict with worktree-locked heads.
	if err := ensureRemoteTrackingLayoutContext(ctx, dest); err != nil {
		if !IsRefCaseConflict(err) && !hasModernRefspecContext(ctx, dest) {
			return fmt.Errorf("configure fetch refspec: %w", err)
		}
		if !IsRefCaseConflict(err) {
			// The download is complete and only the fetch that fills
			// refs/remotes/origin/* failed. Retry it once, then keep the
			// clone anyway, once its default branch is recorded: every later
			// fetch fills the other refs.
			if err = gitFetchContext(ctx, dest); err != nil && !IsRefCaseConflict(err) {
				if recordErr := recordCloneDefaultBranchContext(ctx, dest); recordErr != nil {
					return fmt.Errorf("configure fetch refspec: %w (and the clone's default branch could not be recorded: %v)", err, recordErr)
				}
			}
		}
		if err != nil {
			warn(err)
		}
	}
	return nil
}

func hasModernRefspecContext(ctx context.Context, barePath string) bool {
	refspec, err := readFetchRefspecContext(ctx, barePath)
	return err == nil && refspec == modernFetchRefspec
}

// recordCloneDefaultBranchContext points refs/remotes/origin/HEAD at the
// remote's default branch as `git clone` recorded it in the bare HEAD,
// creating that remote-tracking ref from the clone's copy if no fetch stored
// it. A failed fetch may store only some refs; without this, the default
// branch would resolve to whichever of origin/main or origin/master it
// stored, not the remote's actual default.
func recordCloneDefaultBranchContext(ctx context.Context, barePath string) error {
	bareRef := bareHeadBranchContext(ctx, barePath)
	if bareRef == "" {
		return errors.New("the clone records no default branch")
	}
	originRef := "refs/remotes/origin/" + strings.TrimPrefix(bareRef, "refs/heads/")
	if err := runGitContext(ctx, "-C", barePath, "rev-parse", "--verify", "--quiet", originRef); err != nil {
		commit, err := resolveCommitContext(ctx, barePath, bareRef)
		if err != nil {
			return err
		}
		if out, err := runGitCombinedOutputContext(ctx, "-C", barePath, "update-ref", originRef, commit); err != nil {
			return fmt.Errorf("create %s: %s: %w", originRef, strings.TrimSpace(string(out)), err)
		}
	}
	if out, err := runGitCombinedOutputContext(ctx, "-C", barePath, "symbolic-ref", "refs/remotes/origin/HEAD", originRef); err != nil {
		return fmt.Errorf("point origin/HEAD at %s: %s: %w", originRef, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func cloneTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(CloneTimeoutEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultCloneTimeout
}

// bloblessEnabled reports whether BloblessEnv selects repoURL. Entries are
// compared by repository identity, so HTTPS and SSH spellings match.
func bloblessEnabled(repoURL string) bool {
	identity := legacyBareDirName(repoURL)
	for _, entry := range strings.Split(os.Getenv(BloblessEnv), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" || legacyBareDirName(entry) == identity {
			return true
		}
	}
	return false
}

// cloneFailureHint names the likely fix for a failed clone from Git's output.
func cloneFailureHint(output string, err error, url string) string {
	lower := strings.ToLower(output + " " + err.Error())
	containsAny := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(lower, n) {
				return true
			}
		}
		return false
	}
	switch {
	case containsAny("could not read username", "authentication failed", "permission denied", "terminal prompts disabled"):
		return "Check that the daemon's Git credentials (credential helper, gh login, or SSH key) can read " + url
	case containsAny("timed out", "too slow", "operation too slow", "rpc failed", "unexpected disconnect",
		"connection reset", "failed to connect", "could not resolve host", "http2", "early eof", "recv failure"):
		return fmt.Sprintf("The link to the Git host is too slow or unstable for this clone. The daemon retries on the next sync; to make the clone smaller set %s=%s (or %s=*) for the daemon, or raise %s (default %s)",
			BloblessEnv, url, BloblessEnv, CloneTimeoutEnv, defaultCloneTimeout)
	}
	return ""
}

// runGitFetchOutput runs the fetch behind runGitFetchContext. Tests replace
// it to reproduce output that only a case-insensitive filesystem produces.
var runGitFetchOutput = runGitFetchOutputContext

// runGitFetchOutputContext runs `git fetch origin` in barePath with Git's
// messages in English, so classifyFetchError can recognize them whatever the
// daemon's locale.
func runGitFetchOutputContext(ctx context.Context, barePath string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, repoCacheGitTimeout)
	defer cancel()
	cmd := newGitCommand("-C", barePath, "fetch", "origin")
	cmd.Env = append(cmd.Env, "LC_ALL=C", "LANGUAGE=")
	out, err := processtree.CombinedOutput(cmdCtx, cmd, 5*time.Second)
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("git command timed out after %s: %w", repoCacheGitTimeout, cmdCtx.Err())
	}
	return out, err
}

// classifyFetchError turns a failed fetch into a RefCaseConflictError when
// Git refused only refs that collide by letter case, and into the usual
// "git fetch: <output>" error otherwise. Git's explanation of the conflict is
// one sign; a Git version that words it differently, or does not explain it,
// is recognized when every ref it refused differs only in letter case from a
// ref the repository holds. storedRefs lists those refs as Git prints them
// (origin/<branch>, <tag>); nil skips the second check.
func classifyFetchError(out []byte, err error, storedRefs func() []string) error {
	output := string(out)
	if strings.Contains(strings.Join(strings.Fields(output), " "), "only differ in casing") {
		return &RefCaseConflictError{Refs: rejectedRefs(output)}
	}
	if refs := rejectedRefs(output); len(refs) > 0 && storedRefs != nil && allDifferOnlyInCase(refs, storedRefs()) {
		return &RefCaseConflictError{Refs: refs}
	}
	return fmt.Errorf("git fetch: %s: %w", strings.TrimSpace(output), err)
}

// allDifferOnlyInCase reports whether each of refused matches a ref in stored
// when letter case is ignored, and only then.
func allDifferOnlyInCase(refused, stored []string) bool {
	for _, ref := range refused {
		if !slices.ContainsFunc(stored, func(s string) bool { return s != ref && strings.EqualFold(s, ref) }) {
			return false
		}
	}
	return true
}

// storedRefNames lists barePath's remote-tracking branches and tags the way
// fetch output names them. Errors yield none, so nothing is excused.
func storedRefNames(ctx context.Context, barePath string) []string {
	out, err := runGitOutputContext(ctx, "-C", barePath, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/remotes", "refs/tags")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// rejectedRefs extracts the local ref names from fetch lines such as
// " ! [new branch]  dup  -> origin/dup  (unable to update local ref)".
func rejectedRefs(output string) []string {
	var refs []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "unable to update local ref") {
			continue
		}
		_, after, ok := strings.Cut(line, "-> ")
		if !ok {
			continue
		}
		if fields := strings.Fields(after); len(fields) > 0 {
			refs = append(refs, fields[0])
		}
	}
	return refs
}

// A blobless cache lacks the files of every commit until something fetches
// them. Git would fetch them lazily during a checkout, inside the checkout
// request, where a slow link means the request ends first and the next
// attempt starts the download again. So missing files are fetched by a job
// the cache owns, bounded like a build (stall check plus the total limit),
// and a checkout only waits for it up to checkoutBlobWaitBudget.

// checkoutBlobWaitBudget is how long a checkout waits for the files of its
// base commit before answering with a retryable error. A deadline set with
// WithWaitDeadline shortens it, so the daemon's cache build wait and this one
// together stay under the CLI's 5-minute request limit.
var checkoutBlobWaitBudget = 3 * time.Minute

type waitDeadlineKey struct{}

// WithWaitDeadline makes deadline the latest time a request on ctx waits for
// cache-owned work (a cache build, a file download): the waits of one
// request share it instead of each taking its own budget. The work itself
// carries on past it.
func WithWaitDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, waitDeadlineKey{}, deadline)
}

// WaitContext bounds a wait for cache-owned work by budget and by the
// deadline set with WithWaitDeadline, whichever comes first.
func WaitContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(budget)
	if shared, ok := ctx.Value(waitDeadlineKey{}).(time.Time); ok && shared.Before(deadline) {
		deadline = shared
	}
	return context.WithDeadline(ctx, deadline)
}

// BlobsPendingError reports a file download the checkout stopped waiting for.
// It matches ErrCacheBuilding and unwraps to the reason the wait ended.
type BlobsPendingError struct {
	URL     string
	Ref     string
	Running time.Duration
	Cause   error
}

func (e *BlobsPendingError) Error() string {
	return fmt.Sprintf("files of %s in %s are still being downloaded into the repository cache in the background (running for %s); retry `multica repo checkout` in a few minutes",
		e.Ref, e.URL, e.Running.Round(time.Second))
}

func (e *BlobsPendingError) Is(target error) bool { return target == ErrCacheBuilding }

func (e *BlobsPendingError) Unwrap() error { return e.Cause }

// ensureCheckoutBlobsContext makes sure a blobless cache holds the files of
// ref before a checkout of it, fetching them in a cache-owned job. Any
// problem determining what is missing is left to the checkout, which still
// fetches lazily.
func (c *Cache) ensureCheckoutBlobsContext(ctx context.Context, repoURL, barePath, ref string) error {
	if !isPartialCloneContext(ctx, barePath) {
		return nil
	}
	commit, err := resolveCommitContext(ctx, barePath, ref)
	if err != nil || c.hasCompleteBlobs(barePath, commit) {
		return nil
	}
	missing, err := listMissingObjects(ctx, barePath, commit)
	if err != nil {
		return nil
	}
	if len(missing) == 0 {
		c.markCompleteBlobs(barePath, commit)
		return nil
	}
	job := c.startBlobFetch(barePath, commit)
	waitCtx, cancel := WaitContext(ctx, checkoutBlobWaitBudget)
	defer cancel()
	select {
	case <-job.done:
		if job.err != nil {
			return fmt.Errorf("download files of %s into the repository cache: %w", ref, job.err)
		}
		return nil
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return &BlobsPendingError{URL: repoURL, Ref: ref, Running: time.Since(job.started), Cause: context.Cause(waitCtx)}
	}
}

// prefetchDefaultBranchInBackground starts fetching the default branch's
// missing files, after a blobless build or a fetch that moved the branch,
// without waiting. The job itself finds what is missing.
func (c *Cache) prefetchDefaultBranchInBackground(ctx context.Context, barePath string) {
	if !isPartialCloneContext(ctx, barePath) {
		return
	}
	ref := getRemoteDefaultBranchContext(ctx, barePath)
	if ref == "" {
		return
	}
	commit, err := resolveCommitContext(ctx, barePath, ref)
	if err != nil || c.hasCompleteBlobs(barePath, commit) {
		return
	}
	c.startBlobFetch(barePath, commit)
}

// startBlobFetch starts, or joins, the job fetching commit's missing files
// into barePath. The job does not take the repository lock: it only adds
// objects, which Git writes atomically, and a checkout waiting for it holds
// that lock. Automatic maintenance is off so it never repacks under others.
func (c *Cache) startBlobFetch(barePath, commit string) *buildJob {
	key := barePath + "\x00" + commit
	c.builds.mu.Lock()
	defer c.builds.mu.Unlock()
	if job, ok := c.builds.blobJobs[key]; ok {
		return job
	}
	if c.builds.blobJobs == nil {
		c.builds.blobJobs = make(map[string]*buildJob)
	}
	job := &buildJob{done: make(chan struct{}), started: time.Now()}
	c.builds.blobJobs[key] = job
	go func() {
		timeout := cloneTimeout()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := fetchMissingObjectsContext(ctx, barePath, commit, timeout)
		cancel()
		if err != nil {
			c.logger.Warn("repo cache: file download failed", "path", barePath, "commit", commit, "error", err)
		}
		c.builds.mu.Lock()
		delete(c.builds.blobJobs, key)
		if err == nil {
			c.builds.markCompleteLocked(key)
		}
		c.builds.mu.Unlock()
		job.err = err
		close(job.done)
	}()
	return job
}

func (c *Cache) hasCompleteBlobs(barePath, commit string) bool {
	c.builds.mu.Lock()
	defer c.builds.mu.Unlock()
	_, ok := c.builds.blobsComplete[barePath+"\x00"+commit]
	return ok
}

func (c *Cache) markCompleteBlobs(barePath, commit string) {
	c.builds.mu.Lock()
	defer c.builds.mu.Unlock()
	c.builds.markCompleteLocked(barePath + "\x00" + commit)
}

func (r *buildRegistry) markCompleteLocked(key string) {
	if r.blobsComplete == nil {
		r.blobsComplete = make(map[string]struct{})
	}
	r.blobsComplete[key] = struct{}{}
}

func (c *Cache) forgetCompleteBlobs(barePath string) {
	c.builds.mu.Lock()
	defer c.builds.mu.Unlock()
	for key := range c.builds.blobsComplete {
		if strings.HasPrefix(key, barePath+"\x00") {
			delete(c.builds.blobsComplete, key)
		}
	}
}

// listMissingObjects runs missingObjectsContext for checkouts and downloads.
// Tests replace it to count how often a tree is listed.
var listMissingObjects = missingObjectsContext

// missingObjectsContext lists the objects of commit's tree that barePath
// does not have. --missing=print reports them with a "?" prefix without
// fetching them one by one.
func missingObjectsContext(ctx context.Context, barePath, commit string) ([]string, error) {
	out, err := runGitOutputContext(ctx, "-C", barePath, "rev-list", "--objects", "--missing=print", "--no-walk", commit)
	if err != nil {
		return nil, fmt.Errorf("list missing objects of %s: %w", commit, err)
	}
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		if oid, ok := strings.CutPrefix(strings.TrimSpace(line), "?"); ok && oid != "" {
			missing = append(missing, oid)
		}
	}
	return missing, nil
}

// runBlobFetch runs the batched fetch behind fetchMissingObjectsContext.
// Tests replace it to model a slow link.
var runBlobFetch = runBlobFetchContext

// fetchMissingObjectsContext fetches, in one batch, the objects of commit's
// tree that barePath lacks.
func fetchMissingObjectsContext(ctx context.Context, barePath, commit string, timeout time.Duration) error {
	missing, err := listMissingObjects(ctx, barePath, commit)
	if err != nil || len(missing) == 0 {
		return err
	}
	out, err := runBlobFetch(ctx, barePath, missing, timeout)
	if err != nil {
		return fmt.Errorf("fetch %d missing objects of %s: %s: %w", len(missing), commit, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func runBlobFetchContext(ctx context.Context, barePath string, oids []string, timeout time.Duration) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The same request Git makes for a lazy fetch, for all objects at once.
	cmd := newGitCommand("-C", barePath,
		"-c", "fetch.negotiationAlgorithm=noop",
		"-c", "http.lowSpeedLimit="+cloneLowSpeedLimit,
		"-c", "http.lowSpeedTime="+cloneLowSpeedTime,
		"-c", "maintenance.auto=false",
		"-c", "gc.auto=0",
		"fetch", "origin", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no",
		"--filter="+partialCloneFilter, "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	out, err := processtree.CombinedOutput(cmdCtx, cmd, 5*time.Second)
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("git command timed out after %s: %w", timeout, cmdCtx.Err())
	}
	return out, err
}
