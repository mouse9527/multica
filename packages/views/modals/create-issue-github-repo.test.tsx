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
import {
  GitHubRepoPicker,
  useGitHubCreateGate,
  useGitHubCreateRepo,
  useGitHubCreateStatus,
} from "./create-issue-github-repo";

const mockGetSettings = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: { getProjectGitHubSettings: mockGetSettings },
}));
vi.mock("sonner", () => ({ toast: { error: mockToastError } }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ labelOf: (key: string) => ({ backlog: "Backlog", todo: "Todo", in_review: "In Review" })[key] ?? key }),
}));

const widgets = { resource_id: "r1", repo: "acme/widgets" };
const gadgets = { resource_id: "r2", repo: "acme/gadgets" };
const entryOnly = ["backlog"];

function settingsByProject(byProject: Record<string, ProjectGitHubSettings | null>) {
  mockGetSettings.mockImplementation(async (projectId: string) => byProject[projectId] ?? null);
}

// Renders the picker and the gate the way the create dialog does, and shows
// the create request it would send.
function Harness({
  initialProject,
  initialStatus = "todo",
  enabled = true,
}: {
  initialProject?: string;
  initialStatus?: string;
  enabled?: boolean;
}) {
  const [projectId, setProjectId] = useState(initialProject);
  const [status, setStatus] = useState(initialStatus);
  const [open, setOpen] = useState(false);
  const [sent, setSent] = useState<unknown>(null);
  const state = useGitHubCreateRepo("ws-1", projectId, enabled);
  useGitHubCreateStatus(state, status, setStatus);
  const blocked = useGitHubCreateGate("ws-1", state, status, () => setOpen(true));
  return (
    <>
      {state.phase !== "none" && <GitHubRepoPicker state={state} open={open} onOpenChange={setOpen} />}
      <output data-testid="target">{JSON.stringify(state.target ?? null)}</output>
      <output data-testid="status">{status}</output>
      <output data-testid="sent">{JSON.stringify(sent)}</output>
      <button type="button" onClick={() => setProjectId("multi")}>switch project</button>
      <button type="button" onClick={() => setStatus("in_review")}>pick in review</button>
      <button type="button" onClick={() => !blocked() && setSent({ status, ...state.target })}>create</button>
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

const read = (id: string) => JSON.parse(screen.getByTestId(id).textContent ?? "null");
const target = () => read("target");
const sent = () => read("sent");

describe("GitHub repository of a new issue", () => {
  beforeEach(() => {
    mockGetSettings.mockReset();
    mockToastError.mockReset();
    settingsByProject({
      single: { repos: [widgets], default_resource_id: null, create_statuses: entryOnly },
      multi: { repos: [widgets, gadgets], default_resource_id: null, create_statuses: entryOnly },
      defaulted: { repos: [widgets, gadgets], default_resource_id: "r2", create_statuses: ["backlog", "in_review"] },
      plain: { repos: [], default_resource_id: null, create_statuses: entryOnly },
    });
  });

  it("selects the only repository and starts the issue in a status GitHub can show", async () => {
    const user = userEvent.setup();
    renderHarness({ initialProject: "single" });
    expect(await screen.findByRole("button", { name: /acme\/widgets/ })).toBeInTheDocument();
    expect(target()).toEqual({ repo: "acme/widgets" });
    // The dialog's todo cannot open a GitHub issue without a status label.
    await waitFor(() => expect(screen.getByTestId("status")).toHaveTextContent("backlog"));
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toEqual({ status: "backlog", repo: "acme/widgets" });
  });

  it("keeps a status GitHub can show", async () => {
    renderHarness({ initialProject: "defaulted", initialStatus: "in_review" });
    expect(await screen.findByRole("button", { name: /acme\/gadgets/ })).toBeInTheDocument();
    expect(target()).toEqual({ repo: "acme/gadgets" });
    expect(screen.getByTestId("status")).toHaveTextContent("in_review");
  });

  it("explains a status picked afterwards that GitHub cannot show, and lets Local through", async () => {
    const user = userEvent.setup();
    renderHarness({ initialProject: "single" });
    await screen.findByRole("button", { name: /acme\/widgets/ });
    await user.click(screen.getByRole("button", { name: "pick in review" }));
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toBeNull();
    expect(mockToastError).toHaveBeenCalledWith(
      "A new GitHub issue in this project can't start as “In Review”. Use Backlog, or choose Local.",
    );
    expect(screen.getByTestId("status")).toHaveTextContent("in_review");

    await user.click(screen.getByRole("button", { name: /acme\/widgets/ }));
    await user.click(await screen.findByRole("button", { name: /local \(not synced to github\)/i }));
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toEqual({ status: "in_review", provider: "local" });
  });

  it("does not create while the repositories are loading", async () => {
    const user = userEvent.setup();
    let resolve: (v: ProjectGitHubSettings) => void = () => {};
    mockGetSettings.mockImplementation(() => new Promise((r) => (resolve = r)));
    renderHarness({ initialProject: "single" });
    expect(screen.getByRole("button", { name: /loading repositories/i })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toBeNull();
    expect(mockToastError).toHaveBeenCalledWith("Still loading this project's GitHub repositories");

    resolve({ repos: [widgets], default_resource_id: null, create_statuses: entryOnly });
    expect(await screen.findByRole("button", { name: /acme\/widgets/ })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toEqual({ status: "backlog", repo: "acme/widgets" });
  });

  it("does not create when the repositories could not be read, and retries", async () => {
    const user = userEvent.setup();
    mockGetSettings.mockRejectedValueOnce(new Error("network down"));
    renderHarness({ initialProject: "single" });
    const retry = await screen.findByRole("button", { name: /retry loading repositories/i });
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toBeNull();
    expect(mockToastError).toHaveBeenCalledWith(
      "Couldn't load this project's GitHub repositories. Retrying; you can also choose another project.",
    );
    await waitFor(() => expect(mockGetSettings).toHaveBeenCalledTimes(2));
    expect(await screen.findByRole("button", { name: /acme\/widgets/ })).toBeInTheDocument();
    expect(retry).not.toBeInTheDocument();
  });

  it("retries from the pill", async () => {
    const user = userEvent.setup();
    mockGetSettings.mockRejectedValueOnce(new Error("network down"));
    renderHarness({ initialProject: "single" });
    await user.click(await screen.findByRole("button", { name: /retry loading repositories/i }));
    expect(await screen.findByRole("button", { name: /acme\/widgets/ })).toBeInTheDocument();
  });

  it("asks when several repositories have no default, and offers Local", async () => {
    const user = userEvent.setup();
    renderHarness({ initialProject: "multi" });
    await screen.findByRole("button", { name: /choose repository/i });
    expect(target()).toBeNull();
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toBeNull();
    expect(mockToastError).toHaveBeenCalledWith("Choose a GitHub repository, or Local");

    // The gate opened the picker.
    await user.click(await screen.findByRole("button", { name: /local \(not synced to github\)/i }));
    expect(target()).toEqual({ provider: "local" });
    expect(screen.getByTestId("status")).toHaveTextContent("todo");

    await user.click(screen.getByRole("button", { name: /local \(not synced to github\)/i }));
    await user.click(await screen.findByRole("button", { name: "acme/gadgets" }));
    expect(target()).toEqual({ repo: "acme/gadgets" });
    await waitFor(() => expect(screen.getByTestId("status")).toHaveTextContent("backlog"));
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

  it("offers nothing and creates as before without repositories, without a project, or with the feature off", async () => {
    const user = userEvent.setup();
    for (const project of ["plain", "off"]) {
      // "off": a server with the feature off answers 404, read as null.
      const view = renderHarness({ initialProject: project });
      await waitFor(() => expect(mockGetSettings).toHaveBeenCalledWith(project));
      await waitFor(() => expect(screen.queryByRole("button", { name: /loading repositories/i })).toBeNull());
      expect(screen.queryByRole("button", { name: /repositor|acme/i })).toBeNull();
      await user.click(screen.getByRole("button", { name: "create" }));
      expect(sent()).toEqual({ status: "todo" });
      view.unmount();
    }

    mockGetSettings.mockClear();
    renderHarness({});
    expect(mockGetSettings).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "create" }));
    expect(sent()).toEqual({ status: "todo" });
    expect(mockToastError).not.toHaveBeenCalled();
  });

  it("does not ask for a create that cannot go to GitHub", async () => {
    renderHarness({ initialProject: "single", enabled: false });
    expect(mockGetSettings).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /acme|repositor/i })).toBeNull();
  });
});
