// Modified for the mouse9527/multica fork: GitHub-native issues.

package main

// githubIssueConcurrentIndexCleanups lists the fork's GitHub-native issue
// migrations that build an index concurrently. They are kept out of the
// upstream concurrentIndexCleanups literal so upstream syncs do not conflict on
// it; init merges them in, and the existing cleanup tests cover them exactly as
// they cover upstream entries.
var githubIssueConcurrentIndexCleanups = map[string]string{
	"9001_githubissues_issue_external_number_idx":    "idx_issue_external_number",
	"9002_githubissues_comment_external_id_idx":      "idx_comment_external_external_id",
	"9003_githubissues_write_intent_open_idx": "idx_github_write_intent_open",
}

// Package-level vars, including preMigrationHooks, are initialized before
// init runs, so the hooks are added here rather than derived from the map.
func init() {
	for version, index := range githubIssueConcurrentIndexCleanups {
		concurrentIndexCleanups[version] = index
		preMigrationHooks[version] = cleanupInvalidConcurrentIndexHook(index)
	}
}
