// @vitest-environment node
// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { describe, expect, it } from "vitest";
import { githubIssueLink } from "./issue-link";

describe("githubIssueLink", () => {
  it("links a GitHub issue to its page on GitHub", () => {
    expect(githubIssueLink({ provider: "github", identifier: "acme/widgets#42" })).toEqual({
      label: "acme/widgets#42",
      url: "https://github.com/acme/widgets/issues/42",
    });
    expect(githubIssueLink({ provider: "github", identifier: "my-org/repo.name_v2#7" })?.url).toBe(
      "https://github.com/my-org/repo.name_v2/issues/7",
    );
  });

  it("has no link for a local issue or a server without the feature", () => {
    expect(githubIssueLink({ provider: "local", identifier: "MUL-42" })).toBeNull();
    expect(githubIssueLink({ identifier: "MUL-42" })).toBeNull();
    // A local identifier never turns into a link, whatever it looks like.
    expect(githubIssueLink({ provider: "local", identifier: "acme/widgets#42" })).toBeNull();
  });

  it("has no link when a GitHub issue's identifier is not owner/repo#N", () => {
    for (const identifier of [
      "MUL-42",
      "acme/widgets#0",
      "acme/widgets#",
      "acme#42",
      "acme/widgets/extra#42",
      "-acme/widgets#42",
      "acme/..#42",
      "acme/widgets#42?x=1",
      "javascript:alert(1)//acme/widgets#42",
    ]) {
      expect(githubIssueLink({ provider: "github", identifier })).toBeNull();
    }
  });
});
