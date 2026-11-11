// Added by the mouse9527/multica fork: choosing the GitHub repository of a new
// GitHub-native issue. Upstream has no such file.
import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ProjectGitHubSettings } from "@multica/core/github-issues";
import enModals from "../locales/en/modals.json";
import enIssues from "../locales/en/issues.json";
import { GitHubRepoPicker, useGitHubCreateRepo } from "./create-issue-github-repo";

const mockGetSettings = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: { getProjectGitHubSettings: mockGetSettings },
}));

const widgets = { resource_id: "r1", repo: "acme/widgets" };
const gadgets = { resource_id: "r2", repo: "acme/gadgets" };

function settingsByProject(byProject: Record<string, ProjectGitHubSettings | null>) {
  mockGetSettings.mockImplementation(async (projectId: string) => byProject[projectId] ?? null);
}

// Renders the picker the way the create dialog does, and shows the create
// request fields it would send.
function Harness({ initialProject, enabled = true }: { initialProject?: string; enabled?: boolean }) {
  const [projectId, setProjectId] = useState(initialProject);
  const [open, setOpen] = useState(false);
  const state = useGitHubCreateRepo("ws-1", projectId, enabled);
  return (
    <>
      {state.visible && <GitHubRepoPicker state={state} open={open} onOpenChange={setOpen} />}
      <output data-testid="target">{JSON.stringify(state.target ?? null)}</output>
      <button type="button" onClick={() => setProjectId("multi")}>switch project</button>
    </>
  );
}

function renderHarness(props: React.ComponentProps<typeof Harness>) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <I18nProvider locale="en" resources={{ en: { modals: enModals, issues: enIssues } }}>
      <QueryClientProvider client={qc}>
        <Harness {...props} />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

const target = () => JSON.parse(screen.getByTestId("target").textContent ?? "null");

describe("GitHub repository of a new issue", () => {
  beforeEach(() => {
    mockGetSettings.mockReset();
    settingsByProject({
      single: { repos: [widgets], default_resource_id: null },
      multi: { repos: [widgets, gadgets], default_resource_id: null },
      defaulted: { repos: [widgets, gadgets], default_resource_id: "r2" },
      plain: { repos: [], default_resource_id: null },
    });
  });

  it("selects the only repository of a single-repository project", async () => {
    renderHarness({ initialProject: "single" });
    expect(await screen.findByRole("button", { name: /acme\/widgets/ })).toBeInTheDocument();
    expect(target()).toEqual({ repo: "acme/widgets" });
  });

  it("selects the project's default", async () => {
    renderHarness({ initialProject: "defaulted" });
    expect(await screen.findByRole("button", { name: /acme\/gadgets/ })).toBeInTheDocument();
    expect(target()).toEqual({ repo: "acme/gadgets" });
  });

  it("asks when several repositories have no default, and offers Local", async () => {
    const user = userEvent.setup();
    renderHarness({ initialProject: "multi" });
    await user.click(await screen.findByRole("button", { name: /choose repository/i }));
    expect(target()).toBeNull();

    await user.click(await screen.findByRole("button", { name: /local \(not synced to github\)/i }));
    expect(target()).toEqual({ provider: "local" });

    await user.click(screen.getByRole("button", { name: /local \(not synced to github\)/i }));
    await user.click(await screen.findByRole("button", { name: "acme/gadgets" }));
    expect(target()).toEqual({ repo: "acme/gadgets" });
  });

  it("drops the pick when the project changes", async () => {
    const user = userEvent.setup();
    renderHarness({ initialProject: "single" });
    await user.click(await screen.findByRole("button", { name: /acme\/widgets/ }));
    await user.click(await screen.findByRole("button", { name: /local \(not synced to github\)/i }));
    expect(target()).toEqual({ provider: "local" });

    await user.click(screen.getByRole("button", { name: "switch project" }));
    expect(await screen.findByRole("button", { name: /choose repository/i })).toBeInTheDocument();
    expect(target()).toBeNull();
  });

  it("offers nothing without repositories, without a project, or with the feature off", async () => {
    const plain = renderHarness({ initialProject: "plain" });
    await waitFor(() => expect(mockGetSettings).toHaveBeenCalledWith("plain"));
    expect(screen.queryByRole("button", { name: /repository|acme/i })).toBeNull();
    expect(target()).toBeNull();
    plain.unmount();

    // A server with the feature off answers null (404).
    const off = renderHarness({ initialProject: "off" });
    await waitFor(() => expect(mockGetSettings).toHaveBeenCalledWith("off"));
    expect(screen.queryByRole("button", { name: /repository|acme/i })).toBeNull();
    off.unmount();

    mockGetSettings.mockClear();
    renderHarness({});
    expect(mockGetSettings).not.toHaveBeenCalled();
    expect(target()).toBeNull();
  });

  it("does not ask for a create that cannot go to GitHub", async () => {
    renderHarness({ initialProject: "single", enabled: false });
    expect(mockGetSettings).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /acme/ })).toBeNull();
  });
});
