// Package passwordauth implements email-and-password login for the
// mouse9527/multica fork: argon2id hashing, password rules, and storage of
// credentials in the fork table user_credential.
//
// This package is fork-only and is not part of upstream multica-ai/multica.
// Upstream files carry thin hooks into it; everything else lives here, in its
// own query file, and in migrations numbered by the rule in
// docs/github-issues.md. Design notes are in docs/password-auth.md.
//
// Passwords and hashes never leave this package in errors or log records:
// errors are fixed sentinel values, and Credential hides its hash from fmt,
// slog and encoding/json.
package passwordauth
