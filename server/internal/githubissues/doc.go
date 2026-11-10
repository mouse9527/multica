// Package githubissues implements GitHub-native issues for the
// mouse9527/multica fork: issues whose number and identity come from a GitHub
// repository, with GitHub as the only writer.
//
// This package is fork-only and is not part of upstream multica-ai/multica.
// Upstream files carry thin hooks into it; everything else lives here, in its
// own handler and query files, and in migrations numbered by the rule in
// docs/github-issues.md.
//
// The feature is off by default. It turns on only when
// MULTICA_GITHUB_ISSUES_ENABLED=true and MULTICA_GITHUB_TOKEN is set; while it
// is off, server behavior is identical to upstream.
package githubissues
