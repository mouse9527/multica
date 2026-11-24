// This file belongs to the mouse9527/multica fork (not present upstream): M-259.

package main

import (
	"strings"
	"testing"
)

func TestRepoCheckoutSummaryKeptNoteOnlyWhenKept(t *testing.T) {
	t.Parallel()
	const repoURL = "https://github.com/org/repo.git"
	const anchor = "the head of a pull request, never its base"

	kept := repoCheckoutSummary(repoURL, repoCheckoutResult{Path: "/work/repo", BranchName: "agent/test/old", Kept: "local_work", UncommittedFiles: 1})
	if !strings.Contains(kept, anchor) {
		t.Fatalf("kept checkout summary lacks the delivery note:\n%s", kept)
	}
	if !strings.Contains(kept, "ask if neither settles it") {
		t.Fatalf("kept checkout summary lacks the ask rule:\n%s", kept)
	}

	keptAfterFailedFetch := repoCheckoutSummary(repoURL, repoCheckoutResult{Path: "/work/repo", BranchName: "agent/test/task", Kept: "task_branch", FetchError: "boom"})
	if !strings.Contains(keptAfterFailedFetch, anchor) || !strings.Contains(keptAfterFailedFetch, "Fetch error: boom") {
		t.Fatalf("kept checkout after a failed fetch must carry both the note and the warning:\n%s", keptAfterFailedFetch)
	}

	fresh := repoCheckoutSummary(repoURL, repoCheckoutResult{Path: "/work/repo", BranchName: "agent/test/task"})
	if strings.Contains(fresh, anchor) {
		t.Fatalf("a fresh checkout must not carry the kept-checkout note:\n%s", fresh)
	}
}
