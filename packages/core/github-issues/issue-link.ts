// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import type { Issue } from "../types/issue";

/** Where a GitHub-backed issue lives on GitHub. */
export interface GitHubIssueLink {
  /** "owner/repo#N", as the issue's identifier shows it. */
  label: string;
  url: string;
}

// The server renders a GitHub issue's identifier as "owner/repo#N" from its
// external link (issuekey.Format), so the URL needs no extra request.
const GITHUB_IDENTIFIER = /^([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)\/([A-Za-z0-9._-]+)#([1-9][0-9]*)$/;

/**
 * The GitHub issue behind `issue`, or null for a local issue. A server with
 * the feature off reports every issue as local, so nothing is shown there.
 */
export function githubIssueLink(
  issue: Pick<Issue, "identifier" | "provider">,
): GitHubIssueLink | null {
  if (issue.provider !== "github") return null;
  const match = GITHUB_IDENTIFIER.exec(issue.identifier);
  if (!match) return null;
  const [, owner, repo, number] = match;
  if (repo === "." || repo === "..") return null;
  return {
    label: issue.identifier,
    url: `https://github.com/${owner}/${repo}/issues/${number}`,
  };
}
