// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { copyText } from "@multica/ui/lib/clipboard";
import { toast } from "sonner";
import { renderWithI18n } from "../../test/i18n";
import { GitHubIssueLinkRow } from "./github-issue-link-row";

vi.mock("@multica/ui/lib/clipboard", () => ({ copyText: vi.fn() }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(copyText).mockResolvedValue(true);
});

const githubIssue = {
  provider: "github",
  github: { repo: "acme/widgets", number: 42, url: "https://github.com/acme/widgets/issues/42" },
};

function Grid({ children }: { children: React.ReactNode }) {
  return <div className="grid grid-cols-[auto_1fr]">{children}</div>;
}

describe("GitHubIssueLinkRow", () => {
  it("opens the GitHub issue in a new tab", () => {
    renderWithI18n(<Grid><GitHubIssueLinkRow issue={githubIssue} /></Grid>);
    expect(screen.getByText("GitHub")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "acme/widgets#42" });
    expect(link).toHaveAttribute("href", "https://github.com/acme/widgets/issues/42");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", expect.stringContaining("noopener"));
  });

  it("copies the GitHub link and reports failure", async () => {
    renderWithI18n(<Grid><GitHubIssueLinkRow issue={githubIssue} /></Grid>);
    fireEvent.click(screen.getByRole("button", { name: "Copy GitHub link" }));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Link copied"));
    expect(copyText).toHaveBeenCalledWith("https://github.com/acme/widgets/issues/42");
    vi.mocked(copyText).mockResolvedValue(false);
    fireEvent.click(screen.getByRole("button", { name: "Copy GitHub link" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Failed to copy link"));
  });

  it("renders nothing for a local issue", () => {
    const { container } = renderWithI18n(<GitHubIssueLinkRow issue={{ provider: "local" }} />);
    expect(container).toBeEmptyDOMElement();
    const { container: legacy } = renderWithI18n(<GitHubIssueLinkRow issue={{}} />);
    expect(legacy).toBeEmptyDOMElement();
  });
});
