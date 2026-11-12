// @vitest-environment node
// Added by the mouse9527/multica fork: GitHub-native issue key autolinking.
// Upstream has no such file.

import { describe, expect, it } from "vitest";
import {
  githubIssueKeyFromMentionId,
  isGitHubIssueKey,
  preprocessGitHubIssueKeys,
} from "@multica/ui/markdown";
import { preprocessMarkdown } from "./preprocess";

const LINK = "[mouse9527/multica#21](mention://issue/mouse9527%2Fmultica%2321)";

describe("preprocessGitHubIssueKeys", () => {
  it("rewrites a bare key into an encoded mention link", () => {
    expect(preprocessGitHubIssueKeys("see mouse9527/multica#21 now")).toBe(
      `see ${LINK} now`,
    );
  });

  it("rewrites keys next to prose punctuation and CJK text", () => {
    expect(preprocessGitHubIssueKeys("（mouse9527/multica#21）、a/b.c#3.")).toBe(
      `（${LINK}）、[a/b.c#3](mention://issue/a%2Fb.c%233).`,
    );
  });

  it("skips keys inside inline code and fenced code", () => {
    const inline = "use `mouse9527/multica#21` here";
    const fenced = "```\nmouse9527/multica#21\n```";
    expect(preprocessGitHubIssueKeys(inline)).toBe(inline);
    expect(preprocessGitHubIssueKeys(fenced)).toBe(fenced);
  });

  it("skips keys inside URLs and existing links", () => {
    const url = "https://github.com/mouse9527/multica#21";
    const labelled = "[mouse9527/multica#21](https://example.com)";
    expect(preprocessGitHubIssueKeys(url)).toBe(url);
    expect(preprocessGitHubIssueKeys(labelled)).toBe(labelled);
    expect(preprocessGitHubIssueKeys(LINK)).toBe(LINK);
  });

  it("skips longer paths, hosts, scoped packages and word continuations", () => {
    for (const text of [
      "a/mouse9527/multica#21",
      "github.com/mouse9527/multica#21",
      "@scope/pkg#1",
      "mouse9527/multica#21/files",
      "mouse9527/multica#21abc",
      "mouse9527/multica#21.5",
    ]) {
      expect(preprocessGitHubIssueKeys(text)).toBe(text);
    }
  });

  it("does not touch short forms without an owner", () => {
    for (const text of ["#21", "multica#21", "client-golive#21"]) {
      expect(preprocessGitHubIssueKeys(text)).toBe(text);
    }
  });
});

describe("githubIssueKeyFromMentionId", () => {
  it("decodes an encoded key and rejects UUIDs and local identifiers", () => {
    expect(githubIssueKeyFromMentionId("mouse9527%2Fmultica%2321")).toBe(
      "mouse9527/multica#21",
    );
    expect(
      githubIssueKeyFromMentionId("1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed"),
    ).toBeNull();
    expect(githubIssueKeyFromMentionId("MUL-1")).toBeNull();
    expect(githubIssueKeyFromMentionId("%E0%A4%A")).toBeNull();
    expect(githubIssueKeyFromMentionId("a%2Fb")).toBeNull();
  });

  it("round-trips with isGitHubIssueKey", () => {
    expect(isGitHubIssueKey("mouse9527/multica#21")).toBe(true);
    expect(isGitHubIssueKey("MUL-1")).toBe(false);
    expect(isGitHubIssueKey("multica#21")).toBe(false);
  });
});

describe("preprocessMarkdown with autolinkIssueIdentifiers", () => {
  it("links GitHub keys and local identifiers side by side", () => {
    expect(
      preprocessMarkdown("BIPO-46 and mouse9527/multica#21", {
        cdnDomain: "",
        autolinkIssueIdentifiers: true,
      }),
    ).toBe(`[BIPO-46](mention://issue/BIPO-46) and ${LINK}`);
  });

  it("does not link a local identifier that is part of a GitHub key", () => {
    expect(
      preprocessMarkdown("acme/MUL-1#2", {
        cdnDomain: "",
        autolinkIssueIdentifiers: true,
      }),
    ).toBe("[acme/MUL-1#2](mention://issue/acme%2FMUL-1%232)");
  });

  it("leaves keys alone on the editable path", () => {
    expect(
      preprocessMarkdown("mouse9527/multica#21", { cdnDomain: "" }),
    ).toBe("mouse9527/multica#21");
  });
});
