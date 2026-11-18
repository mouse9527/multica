// Added by the mouse9527/multica fork: Router page settings (M-105) and live
// Super Agent settings (M-147).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import type { Agent, ChatSession } from "@multica/core/types";
import { DropdownMenu, DropdownMenuContent, DropdownMenuTrigger } from "@multica/ui/components/ui/dropdown-menu";
import { renderWithI18n } from "../test/i18n";
import en from "../locales/en/router.json";
import { useRouterChatMenu } from "./use-router-chat-menu";
import { RoutersTab } from "./routers-tab";

const state = vi.hoisted(() => ({
  flag: true,
  userId: "user-1",
  subscriptions: [] as unknown[],
  dispatches: [] as unknown[],
  sessions: [] as unknown[],
  changes: [] as unknown[],
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => state.flag }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: state.userId } }),
}));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-1", name: "Acme" }),
  useWorkspacePaths: () => ({
    chatSession: (id: string) => `/acme/chat?session=${id}`,
    issueDetail: (id: string) => `/acme/issues/${id}`,
  }),
}));
vi.mock("../navigation", () => ({
  AppLink: ({ children, href, className }: { children: React.ReactNode; href: string; className?: string }) => (
    <a href={href} className={className}>
      {children}
    </a>
  ),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
// The issue search is covered by the picker's own tests; here it picks a fixed root.
vi.mock("../modals/issue-picker-modal", () => ({
  IssuePickerModal: ({ open, onSelect, excludeIds }: { open: boolean; onSelect: (issue: unknown) => void; excludeIds: string[] }) =>
    open ? (
      <button
        type="button"
        data-exclude={excludeIds.join(",")}
        onClick={() => onSelect({ id: "issue-root", identifier: "M-1", title: "Root" })}
      >
        pick root
      </button>
    ) : null,
}));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: {
    forkRequest: vi.fn(),
    listProjects: vi.fn(async () => ({ projects: [{ id: "proj-1", title: "Platform" }], total: 1 })),
    getIssue: vi.fn(async (id: string) => ({ id, identifier: id === "issue-root" ? "M-1" : "M-9", title: "Root" })),
    listAgents: vi.fn(async () => [{ id: "agent-1", name: "Mika", owner_id: "user-1" }]),
    listChatSessions: vi.fn(async () => state.sessions),
    createIssue: vi.fn(),
    deleteIssue: vi.fn(),
  },
}));

const forkRequest = vi.mocked(api.forkRequest);

function sub(over: Record<string, unknown> = {}) {
  return {
    id: "sub-1",
    workspace_id: "ws-1",
    agent_id: "agent-1",
    owner_user_id: "user-1",
    scope_type: "issue_tree",
    scope_id: "issue-root",
    anchor_issue_id: "issue-root",
    anchor_chat_session_id: "chat-1",
    event_types: ["issue_created", "status_changed", "assignee_changed", "comment_created", "task_failed"],
    enabled: true,
    paused_reason: null,
    debounce_seconds: 60,
    max_wait_seconds: 600,
    max_runs_per_hour: 6,
    reconciled_through: new Date().toISOString(),
    created_by_type: "member",
    created_by_id: "user-1",
    created_at: "2026-11-16T00:00:00Z",
    event_count: 4,
    capture_miss: 2,
    ...over,
  };
}

const session = {
  id: "chat-1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "user-1",
  title: "Feature lead",
  status: "active",
  has_unread: false,
  created_at: "2026-11-16T00:00:00Z",
  updated_at: "2026-11-16T00:00:00Z",
} as ChatSession;
const agent = { id: "agent-1", name: "Mika" } as Agent;

function routeForkRequest() {
  forkRequest.mockImplementation(async (path: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    if (method === "GET" && path === "/api/router/subscriptions") return { subscriptions: state.subscriptions };
    if (method === "GET" && path.startsWith("/api/router/dispatches")) return { dispatches: state.dispatches };
    if (method === "GET" && path.endsWith("/changes")) return { changes: state.changes };
    if (method === "DELETE") return undefined;
    if (method === "PATCH") {
      // The server returns the subscription as stored: still paused until resumed.
      const id = path.split("/").pop();
      return (state.subscriptions as { id: string }[]).find((s) => s.id === id) ?? sub();
    }
    return sub();
  });
}

function Harness({ chat }: { chat: ChatSession }) {
  const router = useRouterChatMenu(chat, agent);
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger>menu</DropdownMenuTrigger>
        <DropdownMenuContent>{router.menuItem}</DropdownMenuContent>
      </DropdownMenu>
      {router.dialog}
    </>
  );
}

function show(ui: React.ReactElement, queries: { staleTime?: number } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0, ...queries } } });
  return renderWithI18n(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function openMenu() {
  fireEvent.click(screen.getByRole("button", { name: "menu" }));
}

function calls(method: string) {
  return forkRequest.mock.calls
    .filter(([, init]) => ((init as RequestInit | undefined)?.method ?? "GET") === method)
    .map(([path, init]) => ({ path, body: (init as RequestInit).body ? JSON.parse(String((init as RequestInit).body)) : undefined }));
}

beforeEach(() => {
  vi.clearAllMocks();
  state.flag = true;
  state.userId = "user-1";
  state.subscriptions = [];
  state.dispatches = [];
  state.sessions = [session];
  state.changes = [];
  routeForkRequest();
});

describe("Router in the chat menu", () => {
  it("shows nothing while router_chat is off", () => {
    state.flag = false;
    show(<Harness chat={session} />);
    openMenu();
    expect(screen.queryByRole("menuitem")).not.toBeInTheDocument();
    expect(forkRequest).not.toHaveBeenCalled();
  });

  it("shows nothing on someone else's chat", async () => {
    state.userId = "user-2";
    show(<Harness chat={session} />);
    openMenu();
    await waitFor(() => expect(screen.queryByRole("menuitem")).not.toBeInTheDocument());
  });

  it("sets an own chat up as a Router bound to that chat", async () => {
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.setup }));
    const dialog = await screen.findByRole("dialog", { name: en.dialog.title_setup });
    const create = within(dialog).getByRole("button", { name: en.actions.create });
    expect(create).toBeDisabled();

    fireEvent.click(within(dialog).getByRole("button", { name: en.scopes.add_issue_tree }));
    fireEvent.click(screen.getByRole("button", { name: "pick root" }));
    // Only the issue-created event: the subscription gets exactly what is checked.
    for (const label of [en.events.status_changed, en.events.assignee_changed, en.events.comment_created, en.events.task_failed]) {
      fireEvent.click(within(dialog).getByRole("checkbox", { name: label }));
    }
    fireEvent.click(create);

    await waitFor(() => expect(calls("POST")).toHaveLength(1));
    expect(calls("POST")[0]).toEqual({
      path: "/api/router/subscriptions",
      body: {
        scope_type: "issue_tree",
        scope_id: "issue-root",
        agent_id: "agent-1",
        anchor_chat_session_id: "chat-1",
        event_types: ["issue_created"],
        debounce_seconds: 60,
        max_wait_seconds: 600,
        max_runs_per_hour: 20,
      },
    });
    expect(api.createIssue).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("picks up a subscription kept when its Router was released instead of creating it again", async () => {
    // Released with "keep": unbound and paused, still holding (agent, scope).
    state.subscriptions = [sub({ anchor_chat_session_id: null, enabled: false, paused_reason: "manual" })];
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.setup }));
    const dialog = await screen.findByRole("dialog", { name: en.dialog.title_setup });
    fireEvent.click(within(dialog).getByRole("button", { name: en.scopes.add_issue_tree }));
    expect(screen.getByRole("button", { name: "pick root" })).toHaveAttribute("data-exclude", "");
    fireEvent.click(screen.getByRole("button", { name: "pick root" }));
    fireEvent.click(within(dialog).getByRole("button", { name: en.actions.create }));

    await waitFor(() => expect(calls("POST").map((c) => c.path)).toEqual(["/api/router/subscriptions/sub-1/resume"]));
    expect(calls("PATCH")).toEqual([
      {
        path: "/api/router/subscriptions/sub-1",
        body: expect.objectContaining({ anchor_chat_session_id: "chat-1", event_types: expect.any(Array) }),
      },
    ]);
    expect(api.createIssue).not.toHaveBeenCalled();
  });

  it("does not offer a scope the agent already covers in another chat's Router", async () => {
    state.subscriptions = [sub({ anchor_chat_session_id: "chat-other" })];
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.setup }));
    const dialog = await screen.findByRole("dialog", { name: en.dialog.title_setup });
    fireEvent.click(within(dialog).getByRole("button", { name: en.scopes.add_issue_tree }));
    expect(screen.getByRole("button", { name: "pick root" })).toHaveAttribute("data-exclude", "issue-root");
  });

  it("shows a subscription made elsewhere when the dialog opens, without a reload (M-147)", async () => {
    // As in the app, where cached queries never go stale on their own.
    show(<Harness chat={session} />, { staleTime: Infinity });
    openMenu();
    const item = await screen.findByRole("menuitem", { name: en.menu.setup });
    // Meanwhile the chat's agent subscribes this chat through the CLI.
    state.subscriptions = [sub()];
    fireEvent.click(item);
    expect(await screen.findByRole("dialog", { name: en.dialog.title_settings })).toBeInTheDocument();
  });

  it("rejects batching outside the server's limits", async () => {
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.setup }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(en.batching.debounce), { target: { value: "5" } });
    expect(within(dialog).getByRole("alert")).toHaveTextContent(en.batching.invalid);
  });

  it("shows a Router's state and resumes it", async () => {
    state.subscriptions = [sub({ paused_reason: "rate" })];
    state.dispatches = [
      { id: "d1", subscription_id: "sub-1", created_at: new Date().toISOString(), event_count: 3, outcome: "completed" },
    ];
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.settings }));
    const dialog = await screen.findByRole("dialog", { name: en.dialog.title_settings });
    expect(within(dialog).getAllByText(en.status.paused).length).toBeGreaterThan(0);
    expect(within(dialog).getAllByText(en.status.reason.rate).length).toBeGreaterThan(0);
    expect(await within(dialog).findByText(/Completed · Events: 3/)).toBeInTheDocument();
    expect(within(dialog).getByText("2")).toBeInTheDocument();
    expect(within(dialog).getByTestId("router-held-items")).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: en.actions.resume }));
    await waitFor(() => expect(calls("POST").map((c) => c.path)).toEqual(["/api/router/subscriptions/sub-1/resume"]));
  });

  it("explains an automatic pause with the server's reason", async () => {
    state.subscriptions = [sub({ paused_reason: "agent_unavailable" })];
    state.changes = [
      { id: "c1", action: "create", detail: {} },
      { id: "c2", action: "auto_pause", detail: { reason: "agent_unavailable", why: "its Super Agent chat chat-1 was archived." } },
    ];
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.settings }));
    expect(await screen.findByText("its Super Agent chat chat-1 was archived.")).toBeInTheDocument();
  });

  it("releases a Router after confirmation, keeping its subscriptions paused", async () => {
    state.subscriptions = [sub()];
    show(<Harness chat={session} />);
    openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: en.menu.settings }));
    const dialog = await screen.findByRole("dialog", { name: en.dialog.title_settings });
    fireEvent.click(within(dialog).getByRole("button", { name: en.actions.release }));
    const confirm = await screen.findByRole("alertdialog", { name: en.release.title });
    expect(forkRequest.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method)).toEqual([]);
    fireEvent.click(within(confirm).getByRole("radio", { name: en.release.keep }));
    fireEvent.click(within(confirm).getByRole("button", { name: en.release.confirm }));
    await waitFor(() => expect(calls("PATCH")).toHaveLength(1));
    expect(calls("POST").map((c) => c.path)).toEqual(["/api/router/subscriptions/sub-1/pause"]);
    expect(calls("PATCH")[0]).toEqual({ path: "/api/router/subscriptions/sub-1", body: { anchor_chat_session_id: "none" } });
  });
});

describe("Routers settings tab", () => {
  it("lists the member's Routers by chat and links each to its chat", async () => {
    state.sessions = [session];
    state.subscriptions = [
      sub(),
      sub({ id: "sub-2", scope_type: "project", scope_id: "proj-1", anchor_issue_id: "board", enabled: false, paused_reason: "manual" }),
      // Another member's Router: its chat is not in this member's chat list.
      sub({ id: "sub-3", anchor_chat_session_id: "chat-other" }),
      // Not a Router: not bound to a chat.
      sub({ id: "sub-4", anchor_chat_session_id: null }),
    ];
    show(<RoutersTab />);
    const link = await screen.findByRole("link", { name: /Feature lead/ });
    expect(link).toHaveAttribute("href", "/acme/chat?session=chat-1");
    expect(screen.getAllByRole("link")).toHaveLength(1);
    expect(await within(link).findByText("Platform")).toBeInTheDocument();
    expect(await within(link).findByText("Mika")).toBeInTheDocument();
    expect(within(link).getByText(en.status.paused)).toBeInTheDocument();
    expect(within(link).getByText(en.status.reason.manual)).toBeInTheDocument();
  });

  it("lists the member's subscriptions not in a Router and deletes one after confirmation", async () => {
    state.subscriptions = [
      sub({ id: "kept", anchor_chat_session_id: null, enabled: false, paused_reason: "manual" }),
      sub({ id: "someone-elses", anchor_chat_session_id: null, owner_user_id: "user-2" }),
    ];
    show(<RoutersTab />);
    const section = await screen.findByTestId("router-unbound");
    expect(within(section).getAllByRole("listitem")).toHaveLength(1);
    fireEvent.click(within(section).getByRole("button", { name: en.list.delete }));
    const confirm = await screen.findByRole("alertdialog", { name: en.list.delete_title });
    expect(calls("DELETE")).toEqual([]);
    fireEvent.click(within(confirm).getByRole("button", { name: en.list.delete }));
    await waitFor(() => expect(calls("DELETE").map((c) => c.path)).toEqual(["/api/router/subscriptions/kept"]));
  });

  it("explains how to make a Router when there is none", async () => {
    show(<RoutersTab />);
    expect(await screen.findByText(en.list.empty)).toBeInTheDocument();
  });
});
