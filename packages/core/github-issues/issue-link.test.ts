// @vitest-environment node
// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { describe, expect, it } from "vitest";
import { githubIssueLink } from "./issue-link";

const github = (repo: string, number: number) => ({ repo, number, url: "https://example.invalid/ignored" });

describe("githubIssueLink", () => {
  it("links a GitHub issue to its page on GitHub", () => {
    expect(githubIssueLink({ provider: "github", github: github("acme/widgets", 42) })).toEqual({
      label: "acme/widgets#42",
      url: "https://github.com/acme/widgets/issues/42",
    });
    expect(githubIssueLink({ provider: "github", github: github("my-org/repo.name_v2", 7) })?.url).toBe(
      "https://github.com/my-org/repo.name_v2/issues/7",
    );
  });

  it("has no link for a local issue or a server without the feature", () => {
    expect(githubIssueLink({ provider: "local" })).toBeNull();
    expect(githubIssueLink({})).toBeNull();
    // A local issue never turns into a link, whatever it carries.
    expect(githubIssueLink({ provider: "local", github: github("acme/widgets", 42) })).toBeNull();
    expect(githubIssueLink({ provider: "github" })).toBeNull();
    expect(githubIssueLink({ provider: "github", github: null })).toBeNull();
  });

  it("has no link when the github field is not a GitHub repository and issue number", () => {
    for (const [repo, number] of [
      ["acme/widgets", 0],
      ["acme/widgets", -1],
      ["acme/widgets", 1.5],
      ["acme", 42],
      ["acme/widgets/extra", 42],
      ["-acme/widgets", 42],
      ["acme/..", 42],
      ["acme/widgets?x=1", 42],
      ["javascript:alert(1)//acme/widgets", 42],
    ] as const) {
      expect(githubIssueLink({ provider: "github", github: github(repo, number) })).toBeNull();
    }
  });
});
