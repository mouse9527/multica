// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../test/i18n";
import en from "../locales/en/evolution.json";
import { EvolutionTab } from "./evolution-tab";
import { useEvolutionSettingsTabs } from "./use-evolution-settings-tabs";

const state = vi.hoisted(() => ({
  role: "owner" as string,
  status: { agent: null } as unknown,
  flag: true,
}));

vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => state.flag }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/permissions", () => ({ useCurrentMember: () => ({ role: state.role }) }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ agentDetail: (id: string) => `/acme/agents/${id}` }),
}));
vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: (wsId: string) => ({
    queryKey: ["runtimes", wsId],
    queryFn: async () => [{ id: "rt-1", name: "Laptop", status: "online" }],
  }),
}));
vi.mock("../navigation", () => ({
  AppLink: ({ children, href, className }: { children: React.ReactNode; href: string; className?: string }) => (
    <a href={href} className={className}>
      {children}
    </a>
  ),
}));
// The runtime and model pickers have their own tests; here one click picks a runtime.
vi.mock("../runtimes/components/mika-runtime-choice", () => ({
  MikaRuntimeChoice: ({ onChange }: { onChange: (v: { runtimeId: string; model: string }) => void }) => (
    <button type="button" onClick={() => onChange({ runtimeId: "rt-1", model: "" })}>
      pick runtime
    </button>
  ),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: { request: vi.fn() },
}));

const apiRequest = vi.mocked(api.request);

beforeEach(() => {
  state.role = "owner";
  state.status = { agent: null };
  state.flag = true;
  apiRequest.mockReset();
  apiRequest.mockImplementation(async (path: string, init?: RequestInit) => {
    if ((init?.method ?? "GET") === "POST" && path === "/api/evolution/enable") {
      state.status = { agent: { id: "agent-evo", name: "Evolution" } };
      return state.status;
    }
    return state.status;
  });
});

function show(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return renderWithI18n(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function TabsProbe() {
  const tabs = useEvolutionSettingsTabs();
  return <span data-testid="tabs">{tabs.map((t) => `${t.value}:${t.adminOnly}`).join(",")}</span>;
}

describe("useEvolutionSettingsTabs", () => {
  it("adds an admin-only Evolution entry while evolution_v1 is on", () => {
    show(<TabsProbe />);
    expect(screen.getByTestId("tabs").textContent).toBe("evolution:true");
  });

  it("adds nothing and calls nothing while evolution_v1 is off", () => {
    state.flag = false;
    show(<TabsProbe />);
    expect(screen.getByTestId("tabs").textContent).toBe("");
    expect(apiRequest).not.toHaveBeenCalled();
  });
});

describe("EvolutionTab", () => {
  it("lets an owner enable Evolution on a chosen runtime", async () => {
    show(<EvolutionTab />);
    const enable = await screen.findByRole("button", { name: en.enable });
    expect(enable).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "pick runtime" }));
    fireEvent.click(enable);

    await waitFor(() =>
      expect(apiRequest).toHaveBeenCalledWith(
        "/api/evolution/enable",
        expect.objectContaining({ method: "POST" }),
      ),
    );
    const post = apiRequest.mock.calls.find(([path]) => path === "/api/evolution/enable");
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({ runtime_id: "rt-1", language: "en", model: "" });
    expect(await screen.findByText(en.enabled_title)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: en.open_agent })).toHaveAttribute("href", "/acme/agents/agent-evo");
  });

  it("shows the enabled agent", async () => {
    state.status = { agent: { id: "agent-evo", name: "进化" } };
    show(<EvolutionTab />);
    expect(await screen.findByText("进化")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: en.enable })).not.toBeInTheDocument();
  });

  it("tells a plain member to ask an admin", async () => {
    state.role = "member";
    show(<EvolutionTab />);
    expect(await screen.findByText(en.not_enabled_member)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: en.enable })).not.toBeInTheDocument();
  });
});
