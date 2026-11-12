// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useQuery } from "@tanstack/react-query";
import type { Issue } from "@multica/core/types";
import { IssueChip } from "./issue-chip";

vi.mock("@tanstack/react-query", () => ({
  useQuery: vi.fn(),
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ iconOf: () => null, colorOf: () => null }),
}));

vi.mock("@multica/core/issues/queries", () => ({
  issueListOptions: () => ({ queryKey: ["issues"] }),
  issueDetailOptions: (_workspaceId: string, issueId: string) => ({
    queryKey: ["issue", issueId],
  }),
}));

vi.mock("./status-icon", () => ({
  StatusIcon: () => <svg data-testid="status-icon" />,
}));

const githubIssue: Partial<Issue> = {
  id: "issue-31",
  identifier: "M-31",
  title: "Backed by a GitHub issue",
  status: "todo",
  provider: "github",
  github: { repo: "acme/widgets", number: 23, url: "https://github.com/acme/widgets/issues/23" },
};

describe("IssueChip for a GitHub-backed issue", () => {
  it("names the issue by its workspace key, like a local one", () => {
    vi.mocked(useQuery).mockImplementation(((options: { queryKey?: readonly unknown[] }) =>
      options.queryKey?.[0] === "issues" ? { data: [githubIssue] } : { data: undefined }) as typeof useQuery);

    render(<IssueChip issueId="issue-31" fallbackLabel="M-31" />);

    expect(screen.getByText("M-31")).toBeInTheDocument();
    expect(screen.getByText("Backed by a GitHub issue")).toBeInTheDocument();
    expect(screen.queryByText(/acme\/widgets#23/)).not.toBeInTheDocument();
  });
});
