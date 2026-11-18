// Added by the mouse9527/multica fork: Router marker in the chat list (M-108).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import type { Agent, ChatSession } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import en from "../locales/en/router.json";
import { RouterChatListMarker } from "./router-chat-marker";
import { useRouterChatMenu } from "./use-router-chat-menu";

const state = vi.hoisted(() => ({ flag: true, chats: [] as unknown[], subscriptions: [] as unknown[] }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => state.flag }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("../navigation", () => ({
  AppLink: ({ children, href }: { children: React.ReactNode; href: string }) => <a href={href}>{children}</a>,
}));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: {
    request: vi.fn(),
    listProjects: vi.fn(async () => ({ projects: [], total: 0 })),
    getIssue: vi.fn(async (id: string) => ({ id, identifier: "M-1", title: "Root" })),
  },
}));

const request = vi.mocked(api.request);

function scope(over: Record<string, unknown> = {}) {
  return {
    subscription_id: "sub-1",
    scope_type: "project",
    scope_id: "proj-1",
    name: "multica",
    identifier: "",
    visible: true,
    paused_reason: null,
    ...over,
  };
}

function show(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return renderWithI18n(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function chatsRequests() {
  return request.mock.calls.filter(([path]) => path === "/api/router/chats");
}

beforeEach(() => {
  vi.clearAllMocks();
  state.flag = true;
  state.chats = [];
  state.subscriptions = [];
  request.mockImplementation(async (path: string) => {
    if (path === "/api/router/chats") return { chats: state.chats };
    if (path === "/api/router/subscriptions") return { subscriptions: state.subscriptions };
    if (path.startsWith("/api/router/dispatches")) return { dispatches: [] };
    if (path.endsWith("/changes")) return { changes: [] };
    return undefined;
  });
});

describe("Router marker in the chat list", () => {
  it("renders nothing and asks nothing while router_chat is off", () => {
    state.flag = false;
    show(<RouterChatListMarker sessionId="chat-1" />);
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
  });

  it("marks only Router chats, with one request for the whole list", async () => {
    state.chats = [{ chat_session_id: "chat-1", paused_reasons: [], scopes: [scope()] }];
    show(
      <>
        <RouterChatListMarker sessionId="chat-1" />
        <RouterChatListMarker sessionId="chat-2" />
        <RouterChatListMarker sessionId="chat-3" />
      </>,
    );
    const marker = await screen.findByRole("img", { name: "Super Agent · Scopes: multica project · Running" });
    expect(marker).toHaveAttribute("data-router-status", "running");
    expect(screen.getAllByRole("img")).toHaveLength(1);
    expect(chatsRequests()).toHaveLength(1);
  });

  it("greys a paused Router and names every scope, its pause reasons, and hidden scopes as unavailable", async () => {
    state.chats = [
      {
        chat_session_id: "chat-1",
        paused_reasons: ["rate"],
        scopes: [
          scope(),
          scope({ subscription_id: "sub-2", scope_type: "issue_tree", scope_id: "i-1", name: "Root", identifier: "M-1", paused_reason: "rate" }),
          scope({ subscription_id: "sub-3", name: "", visible: false }),
        ],
      },
    ];
    show(<RouterChatListMarker sessionId="chat-1" />);
    const marker = await screen.findByRole("img");
    expect(marker).toHaveAttribute("data-router-status", "paused");
    expect(marker).toHaveAccessibleName(
      `Super Agent · Scopes: multica project, M-1 Root (paused), ${en.scopes.unknown} · Paused (${en.status.reason.rate})`,
    );
  });

  it("lists every reason without marking single scopes when the whole Router is paused", async () => {
    state.chats = [
      {
        chat_session_id: "chat-1",
        paused_reasons: ["manual", "agent_unavailable"],
        scopes: [scope({ paused_reason: "manual" }), scope({ subscription_id: "sub-2", name: "web", paused_reason: "agent_unavailable" })],
      },
    ];
    show(<RouterChatListMarker sessionId="chat-1" />);
    expect(await screen.findByRole("img")).toHaveAccessibleName(
      `Super Agent · Scopes: multica project, web project · Paused (${en.status.reason.manual}, ${en.status.reason.agent_unavailable})`,
    );
  });
});

const session = {
  id: "chat-1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "user-1",
  title: "人机交互问题",
  status: "active",
  has_unread: false,
  created_at: "2026-11-16T00:00:00Z",
  updated_at: "2026-11-16T00:00:00Z",
} as ChatSession;

function Header({ chat }: { chat: ChatSession }) {
  const router = useRouterChatMenu(chat, { id: "agent-1", name: "Mika" } as Agent);
  return (
    <>
      {router.marker}
      {router.dialog}
    </>
  );
}

describe("Router marker in the chat header", () => {
  it("opens the Router settings", async () => {
    state.chats = [{ chat_session_id: "chat-1", paused_reasons: [], scopes: [scope()] }];
    state.subscriptions = [
      {
        id: "sub-1",
        agent_id: "agent-1",
        owner_user_id: "user-1",
        scope_type: "project",
        scope_id: "proj-1",
        anchor_issue_id: "board-1",
        anchor_chat_session_id: "chat-1",
      },
    ];
    show(<Header chat={session} />);
    fireEvent.click(await screen.findByRole("button", { name: "Super Agent · Scopes: multica project · Running" }));
    expect(await screen.findByRole("dialog", { name: en.dialog.title_settings })).toBeInTheDocument();
  });

  it("is absent on a chat that is not a Router", async () => {
    show(<Header chat={session} />);
    await waitFor(() => expect(chatsRequests()).toHaveLength(1));
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
