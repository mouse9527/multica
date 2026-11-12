// Added by the mouse9527/multica fork: GitHub-native issue keys
// (`owner/repo#N`) in readonly content. Upstream has no such file.
//
// The real preprocessing, RichLink dispatch and identifier lookup run; only the
// API client and the chip are stubbed, so a chip here means the key went all
// the way to a lookup that hit.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Issue } from "@multica/core/types";

const getIssue = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", async () => {
  const { ApiError } = await vi.importActual<typeof import("@multica/core/api")>(
    "@multica/core/api",
  );
  return {
    api: { getIssue, getAttachmentTextContent: vi.fn() },
    ApiError,
    PreviewTooLargeError: class extends Error {},
    PreviewUnsupportedError: class extends Error {},
  };
});

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof editor) => string) => select(editor),
    }),
    useTimeAgo: () => "just now",
  };
});

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme", issue_prefix: "BIPO" }),
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/acme/issues/${id}`,
    projectDetail: (id: string) => `/acme/projects/${id}`,
  }),
  useWorkspaceSlug: () => "acme",
}));

vi.mock("../issues/components/issue-mention-card", () => ({
  IssueMentionCard: ({
    issueId,
    fallbackLabel,
  }: {
    issueId: string;
    fallbackLabel?: string;
  }) => (
    <a data-testid="issue-chip" href={`/acme/issues/${issueId}`}>
      {fallbackLabel}
    </a>
  ),
}));

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

import { ApiError } from "@multica/core/api";
import { RichContent } from "./rich-content";

const TRACKED_ID = "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed";
const LOCAL_ID = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";

const ISSUES: Record<string, Issue> = {
  "mouse9527/multica#21": { id: TRACKED_ID, identifier: "mouse9527/multica#21" } as Issue,
  "BIPO-46": { id: LOCAL_ID, identifier: "BIPO-46" } as Issue,
};

beforeEach(() => {
  getIssue.mockReset();
  getIssue.mockImplementation(async (id: string) => {
    const issue = ISSUES[id];
    if (!issue) throw new ApiError("issue not found", 404, "Not Found");
    return issue;
  });
});

function renderContent(content: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <RichContent content={content} />
    </QueryClientProvider>,
  );
}

describe("GitHub issue keys in readonly content", () => {
  it("renders a tracked owner/repo#N as a chip linking to the issue", async () => {
    const { findByTestId } = renderContent("Blocked by mouse9527/multica#21.");

    const chip = await findByTestId("issue-chip");
    expect(chip.textContent).toBe("mouse9527/multica#21");
    expect(chip.getAttribute("href")).toBe(`/acme/issues/${TRACKED_ID}`);
    expect(getIssue).toHaveBeenCalledWith("mouse9527/multica#21");
  });

  it("leaves an untracked owner/repo#N as the plain text the author wrote", async () => {
    const { container, queryByTestId } = renderContent("See other/repo#9 too.");

    await waitFor(() => expect(getIssue).toHaveBeenCalledWith("other/repo#9"));
    expect(queryByTestId("issue-chip")).toBeNull();
    expect(container.textContent).toBe("See other/repo#9 too.");
    expect(container.querySelector("a")).toBeNull();
  });

  it("does not convert keys inside code blocks or inline code", () => {
    const { container, queryByTestId } = renderContent(
      "Run `mouse9527/multica#21` here\n\n```\nmouse9527/multica#21\n```",
    );

    expect(queryByTestId("issue-chip")).toBeNull();
    expect(getIssue).not.toHaveBeenCalled();
    expect(container.querySelectorAll("code").length).toBeGreaterThanOrEqual(2);
  });

  it("renders a GitHub key and a local identifier side by side", async () => {
    const { findAllByTestId, container } = renderContent(
      "BIPO-46 waits on mouse9527/multica#21, not #22.",
    );

    await waitFor(async () =>
      expect((await findAllByTestId("issue-chip")).length).toBe(2),
    );
    const chips = await findAllByTestId("issue-chip");
    expect(chips.map((c) => c.textContent)).toEqual([
      "BIPO-46",
      "mouse9527/multica#21",
    ]);
    expect(container.textContent).toContain("not #22.");
    expect(getIssue).not.toHaveBeenCalledWith("#22");
  });
});
