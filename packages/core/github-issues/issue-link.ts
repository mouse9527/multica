// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import type { Issue } from "../types/issue";

/** Where a GitHub-backed issue lives on GitHub. */
export interface GitHubIssueLink {
  /** "owner/repo#N". */
  label: string;
  url: string;
}

// The server sends a GitHub issue's repository and number in its github
// field; the URL is built from them so only a github.com issue page is linked.
const GITHUB_REPO = /^([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)\/([A-Za-z0-9._-]+)$/;

/**
 * The GitHub issue behind `issue`, or null for a local issue. A server with
 * the feature off reports every issue as local, so nothing is shown there.
 */
export function githubIssueLink(
  issue: Pick<Issue, "provider" | "github">,
): GitHubIssueLink | null {
  if (issue.provider !== "github" || !issue.github) return null;
  const { repo, number } = issue.github;
  const match = GITHUB_REPO.exec(repo);
  if (!match || !Number.isSafeInteger(number) || number <= 0) return null;
  const [, owner, name] = match;
  if (name === "." || name === "..") return null;
  return {
    label: `${owner}/${name}#${number}`,
    url: `https://github.com/${owner}/${name}/issues/${number}`,
  };
}
