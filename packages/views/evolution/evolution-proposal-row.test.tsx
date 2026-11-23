// Added by the mouse9527/multica fork: apply and roll back an Evolution proposal (M-138).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../test/i18n";
import en from "../locales/en/evolution.json";
import { EvolutionProposalRow } from "./evolution-proposal-row";

const state = vi.hoisted(() => ({ flag: true, proposal: null as unknown, refuse: null as Error | null }));

vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => state.flag }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: { request: vi.fn() },
}));

const apiRequest = vi.mocked(api.request);
const proposalMeta = { evolution_fingerprint: "fp-1" };

function proposal(overrides: Record<string, unknown> = {}) {
  return {
    issue_id: "issue-1",
    target_type: "agent",
    target_id: "agent-1",
    target_name: "Reviewer",
    field: "instructions",
    change: { before: "Close when merged.", after: "Close after checking the PR is merged." },
    state: "pending",
    applicable: true,
    stale: false,
    changed_since_apply: false,
    can_apply: true,
    can_rollback: false,
    versions: [],
    ...overrides,
  };
}

const appliedVersion = { id: "v1", version: 1, action: "apply", field: "instructions", actor_id: "u1", created_at: "" };

beforeEach(() => {
  state.flag = true;
  state.proposal = proposal();
  state.refuse = null;
  vi.mocked(toast.success).mockReset();
  vi.mocked(toast.error).mockReset();
  apiRequest.mockReset();
  apiRequest.mockImplementation(async (path: string, init?: RequestInit) => {
    if ((init?.method ?? "GET") === "POST") {
      if (state.refuse) throw state.refuse;
      state.proposal = path.endsWith("/apply")
        ? proposal({ state: "applied", can_apply: false, can_rollback: true, versions: [appliedVersion] })
        : proposal({ state: "rolled_back", can_apply: true, versions: [appliedVersion] });
    }
    return state.proposal;
  });
});

function show(metadata: Record<string, unknown> | null = proposalMeta) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <EvolutionProposalRow issueId="issue-1" metadata={metadata} />
    </QueryClientProvider>,
  );
}

describe("EvolutionProposalRow", () => {
  it("renders and asks nothing while evolution_v1 is off", () => {
    state.flag = false;
    const { container } = show();
    expect(container.textContent).toBe("");
    expect(apiRequest).not.toHaveBeenCalled();
  });

  it("asks nothing about issues Evolution did not file", () => {
    const { container } = show({ other: "x" });
    expect(container.textContent).toBe("");
    expect(apiRequest).not.toHaveBeenCalled();
  });

  it("applies after confirming the change the server stored", async () => {
    const user = userEvent.setup();
    show();
    expect(await screen.findByText(en.proposal.pending)).toBeTruthy();
    expect(apiRequest).toHaveBeenCalledWith(
      "/api/evolution/proposals/issue-1",
      expect.objectContaining({ headers: expect.objectContaining({ "X-Workspace-ID": "ws-1" }) }),
    );

    await user.click(screen.getByRole("button", { name: en.proposal.apply }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Close after checking the PR is merged.")).toBeTruthy();
    expect(within(dialog).getByText("Close when merged.")).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: en.proposal.apply }));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(en.proposal.applied_toast));
    expect(apiRequest).toHaveBeenCalledWith(
      "/api/evolution/proposals/issue-1/apply",
      expect.objectContaining({ method: "POST" }),
    );
    expect(await screen.findByRole("button", { name: en.proposal.rollback })).toBeTruthy();
    expect(screen.getByText(en.proposal.applied.replace("{{version}}", "1"))).toBeTruthy();
  });

  it("shows the server's reason when a change is refused, and keeps the dialog", async () => {
    const user = userEvent.setup();
    state.refuse = new Error("the agent changed after this proposal was written");
    show();
    await user.click(await screen.findByRole("button", { name: en.proposal.apply }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: en.proposal.apply }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("the agent changed after this proposal was written"));
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("offers nothing to members who may not change it", async () => {
    state.proposal = proposal({ can_apply: false });
    show();
    expect(await screen.findByText(en.proposal.pending)).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("says when a proposal is out of date or carried out by hand", async () => {
    state.proposal = proposal({ stale: true, can_apply: false });
    const { unmount } = show();
    expect(await screen.findByText(en.proposal.stale)).toBeTruthy();
    unmount();

    state.proposal = proposal({ target_type: "squad", applicable: false, can_apply: false });
    show();
    expect(await screen.findByText(en.proposal.by_hand)).toBeTruthy();
  });
});
