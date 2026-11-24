// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136, M-248).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../test/i18n";
import en from "../locales/en/evolution.json";
import { EvolutionTab } from "./evolution-tab";
import { useEvolutionSettingsTabs } from "./use-evolution-settings-tabs";

const state = vi.hoisted(() => ({
  role: "owner" as string,
  status: { agent: null } as unknown,
  config: null as unknown,
  flag: true,
  projects: [] as { id: string; title: string; archived_at: string | null; visibility?: string }[],
}));

vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => state.flag }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/permissions", () => ({ useCurrentMember: () => ({ role: state.role }) }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    agentDetail: (id: string) => `/acme/agents/${id}`,
    issueDetail: (id: string) => `/acme/issues/${id}`,
  }),
}));
vi.mock("@multica/core/projects", () => ({
  projectListOptions: (wsId: string) => ({
    queryKey: ["projects", wsId],
    queryFn: async () => state.projects,
  }),
}));
vi.mock("@multica/core/workspace/queries", async (original) => ({
  ...(await original<typeof import("@multica/core/workspace/queries")>()),
  agentListOptions: (wsId: string) => ({
    queryKey: ["agents", wsId],
    queryFn: async () => [
      { id: "agent-evo", name: "Evolution", archived_at: null },
      { id: "agent-2", name: "Builder", archived_at: null },
    ],
  }),
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

function configState(
  overrides: { chat_active?: boolean; chat?: boolean; enabled?: boolean; output_project_id?: string } = {},
) {
  return {
    config: {
      enabled: overrides.enabled ?? false,
      schedule: { time: "03:00", timezone: "Asia/Shanghai", interval_days: 1 },
      sources: { comments: true, chat: overrides.chat ?? false },
      exclude_projects: [],
      exclude_agents: [],
      max_items_per_window: 2000,
      max_window_days: 7,
      max_runs_per_day: 3,
      output_project_id: overrides.output_project_id ?? "",
    },
    chat_active: overrides.chat_active ?? false,
    comments_active: false,
    limits: { items_per_window: [50, 5000], window_days: [1, 30], runs_per_day: [1, 10], interval_days: [1, 7], max_excluded: 200 },
    runs: [],
  };
}

beforeEach(() => {
  state.role = "owner";
  state.status = { agent: null };
  state.config = configState();
  state.flag = true;
  state.projects = [{ id: "p-1", title: "Website", archived_at: null }];
  apiRequest.mockReset();
  apiRequest.mockImplementation(async (path: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    if (method === "POST" && path === "/api/evolution/enable") {
      state.status = { agent: { id: "agent-evo", name: "Evolution" } };
      return state.status;
    }
    if (path === "/api/evolution/config") {
      if (method === "PUT") {
        const body = JSON.parse(String(init?.body));
        const turnsChatOn = body.config.enabled && body.config.sources.chat;
        if (turnsChatOn && !body.acknowledge_member_notice) throw new Error("confirm with acknowledge_member_notice");
        state.config = { ...configState(), config: body.config, chat_active: turnsChatOn };
      }
      return state.config;
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

describe("EvolutionConfigSection", () => {
  beforeEach(() => {
    state.status = { agent: { id: "agent-evo", name: "Evolution" } };
  });

  const puts = () => apiRequest.mock.calls.filter(([path, init]) => path === "/api/evolution/config" && init?.method === "PUT");

  it("asks before turning chat reading on, then saves with the confirmation", async () => {
    const user = userEvent.setup();
    show(<EvolutionTab />);
    await user.click(await screen.findByRole("switch", { name: en.config_title }));
    await user.click(screen.getByRole("switch", { name: en.chat_label }));
    expect(screen.getByText(en.chat_pending)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: en.config_save }));

    expect(await screen.findByText(en.notice_title)).toBeInTheDocument();
    expect(puts()).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: en.notice_confirm }));

    await waitFor(() => expect(puts()).toHaveLength(1));
    const body = JSON.parse(String(puts()[0]?.[1]?.body));
    expect(body.acknowledge_member_notice).toBe(true);
    expect(body.config).toMatchObject({ enabled: true, sources: { comments: true, chat: true } });
    expect(await screen.findByText(en.chat_active)).toBeInTheDocument();
  });

  it("saves without asking when chat reading is not being turned on", async () => {
    const user = userEvent.setup();
    show(<EvolutionTab />);
    await user.click(await screen.findByRole("switch", { name: en.config_title }));
    await user.click(screen.getByRole("button", { name: en.config_save }));
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(screen.queryByText(en.notice_title)).not.toBeInTheDocument();
    expect(JSON.parse(String(puts()[0]?.[1]?.body))).toMatchObject({
      acknowledge_member_notice: false,
      config: { enabled: true, sources: { chat: false } },
    });
  });

  it("offers other agents, not Evolution itself, as exclusions", async () => {
    show(<EvolutionTab />);
    const agents = within(await screen.findByRole("group", { name: en.exclude_agents }));
    expect(await agents.findByText("Builder")).toBeInTheDocument();
    expect(agents.queryByText("Evolution")).not.toBeInTheDocument();
    const projects = within(screen.getByRole("group", { name: en.exclude_projects }));
    expect(await projects.findByText("Website")).toBeInTheDocument();
  });

  it("shows members the settings read-only", async () => {
    state.role = "member";
    state.config = configState({ enabled: true, chat: true, chat_active: true });
    show(<EvolutionTab />);
    expect(await screen.findByText(en.chat_active)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: en.chat_label })).toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByRole("button", { name: en.config_save })).not.toBeInTheDocument();
  });

  describe("output project", () => {
    const picker = () => screen.findByRole("combobox", { name: en.output_project });

    beforeEach(() => {
      state.projects = [
        { id: "p-1", title: "Website", archived_at: null, visibility: "workspace" },
        { id: "p-evo", title: "Evolution inbox", archived_at: null, visibility: "workspace" },
        { id: "p-old", title: "Legacy", archived_at: "2026-11-01T00:00:00Z", visibility: "workspace" },
        { id: "p-secret", title: "Secret", archived_at: null, visibility: "private" },
      ];
    });

    it("offers only public, unarchived projects and saves the chosen one", async () => {
      const user = userEvent.setup();
      show(<EvolutionTab />);
      expect(await picker()).toHaveTextContent(en.output_project_none);
      await user.click(await picker());
      expect(await screen.findByRole("option", { name: "Website" })).toBeInTheDocument();
      expect(screen.getByRole("option", { name: en.output_project_none })).toBeInTheDocument();
      expect(screen.queryByRole("option", { name: "Legacy" })).toBeNull();
      expect(screen.queryByRole("option", { name: "Secret" })).toBeNull();
      await user.click(screen.getByRole("option", { name: "Evolution inbox" }));
      expect(await picker()).toHaveTextContent("Evolution inbox");

      await user.click(screen.getByRole("button", { name: en.config_save }));
      await waitFor(() => expect(puts()).toHaveLength(1));
      expect(JSON.parse(String(puts()[0]?.[1]?.body)).config.output_project_id).toBe("p-evo");
    });

    it("clears the project with No project", async () => {
      const user = userEvent.setup();
      state.config = configState({ output_project_id: "p-evo" });
      show(<EvolutionTab />);
      const trigger = await picker();
      await waitFor(() => expect(trigger).toHaveTextContent("Evolution inbox"));
      expect(screen.queryByRole("alert")).toBeNull();
      await user.click(trigger);
      await user.click(await screen.findByRole("option", { name: en.output_project_none }));

      await user.click(screen.getByRole("button", { name: en.config_save }));
      await waitFor(() => expect(puts()).toHaveLength(1));
      expect(JSON.parse(String(puts()[0]?.[1]?.body)).config.output_project_id).toBe("");
    });

    it.each([
      ["p-old", "Legacy", en.output_project_archived],
      ["p-secret", "Secret", en.output_project_private],
      ["p-gone", en.output_project_unknown, en.output_project_missing],
    ])("warns when the chosen project %s can no longer hold Evolution's issues", async (id, shown, warning) => {
      state.config = configState({ output_project_id: id });
      show(<EvolutionTab />);
      expect(await screen.findByRole("alert")).toHaveTextContent(warning);
      expect(await picker()).toHaveTextContent(shown);
      expect(await picker()).toHaveAttribute("aria-invalid", "true");
    });

    it("is read-only for members", async () => {
      state.role = "member";
      show(<EvolutionTab />);
      expect(await picker()).toBeDisabled();
    });
  });
});
