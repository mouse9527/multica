// @vitest-environment jsdom
// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Agent } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enMcpOAuth from "../locales/en/mcpoauth.json";
import {
  AgentMcpOAuthConnection,
  AgentMcpOAuthOwnerNote,
  WorkspaceMcpOAuthConnection,
} from "./mcp-oauth-rows";

const lists = vi.hoisted(() => ({
  agent: { servers: [] as Array<Record<string, unknown>>, can_connect: true, available: true },
  workspace: { servers: [] as Array<Record<string, unknown>>, available: true },
}));
const mockConnectAgent = vi.hoisted(() => vi.fn());
const mockDisconnectAgent = vi.hoisted(() => vi.fn());
const mockConnectWorkspace = vi.hoisted(() => vi.fn());
const mockDisconnectWorkspace = vi.hoisted(() => vi.fn());
const mockOpenExternal = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/mcp-oauth", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/mcp-oauth")>("@multica/core/mcp-oauth");
  return {
    ...actual,
    agentMcpOAuthOptions: (wsId: string, agentId: string, enabled: boolean, version: string) => ({
      queryKey: ["mcp-oauth", wsId, "agent", agentId, version],
      queryFn: () => Promise.resolve(lists.agent),
      enabled,
    }),
    workspaceMcpOAuthOptions: (wsId: string, enabled: boolean, version: string) => ({
      queryKey: ["mcp-oauth", wsId, "workspace", version],
      queryFn: () => Promise.resolve(lists.workspace),
      enabled,
    }),
    useConnectAgentMcpOAuth: () => ({ mutateAsync: mockConnectAgent }),
    useDisconnectAgentMcpOAuth: () => ({ mutateAsync: mockDisconnectAgent }),
    useConnectWorkspaceMcpOAuth: () => ({ mutateAsync: mockConnectWorkspace }),
    useDisconnectWorkspaceMcpOAuth: () => ({ mutateAsync: mockDisconnectWorkspace }),
  };
});

vi.mock("@multica/core/workspace/queries", () => ({
  agentMcpServersOptions: (agentId: string) => ({
    queryKey: ["agents", agentId, "mcp-servers"],
    queryFn: () => Promise.resolve([]),
  }),
  workspaceMcpServersOptions: (wsId: string) => ({
    queryKey: ["workspaces", wsId, "mcp-servers"],
    queryFn: () => Promise.resolve([]),
  }),
}));

vi.mock("../navigation", () => ({
  useNavigation: () => ({
    pathname: "/acme/agents/agent-1",
    searchParams: new URLSearchParams("view=mcp_config"),
    hash: "",
    replace: vi.fn(),
    push: vi.fn(),
  }),
  currentPath: (nav: { pathname: string; searchParams: URLSearchParams; hash: string }) =>
    `${nav.pathname}?${nav.searchParams.toString()}${nav.hash}`,
}));

vi.mock("../platform", () => ({
  openExternal: mockOpenExternal,
  isDesktopShell: () => false,
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), message: vi.fn() },
}));

const agent = { id: "agent-1", workspace_id: "ws-1", name: "Helper", mcp_config: {} } as unknown as Agent;

function Shell({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <I18nProvider locale="en" resources={{ en: { mcpoauth: enMcpOAuth } }}>
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    </I18nProvider>
  );
}

const connected = { status: "connected", connected_at: "x", expires_at: null, last_error: "", refreshed_at: null };
const needsReconnect = { ...connected, status: "needs_reconnect", last_error: "url_changed" };

const agentEntry = (over: Record<string, unknown>) => ({
  name: "gateway",
  source: "agent",
  workspace_server_id: "",
  scope: "",
  shared: false,
  problem: "",
  connection: null,
  ...over,
});

beforeEach(() => {
  vi.clearAllMocks();
  lists.agent = { servers: [], can_connect: true, available: true };
  lists.workspace = { servers: [], available: true };
});

describe("AgentMcpOAuthConnection", () => {
  it("shows the state of the agent's own entry and lets the owner connect it", async () => {
    lists.agent.servers = [agentEntry({ name: "gateway" })];
    mockConnectAgent.mockResolvedValue("https://auth.example.com/authorize?state=x");
    render(<AgentMcpOAuthConnection agent={agent} canEdit name="gateway" />, { wrapper: Shell });

    expect(await screen.findByText("Not connected")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Disconnect gateway" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Connect gateway" }));
    await waitFor(() =>
      expect(mockOpenExternal).toHaveBeenCalledWith("https://auth.example.com/authorize?state=x", { webTarget: "same-tab" }),
    );
    expect(mockConnectAgent).toHaveBeenCalledWith({ name: "gateway", returnTo: "/acme/agents/agent-1?view=mcp_config" });
  });

  it("offers a reconnect and a disconnect for a connection that needs it", async () => {
    lists.agent.servers = [agentEntry({ connection: needsReconnect })];
    mockDisconnectAgent.mockResolvedValue(undefined);
    render(<AgentMcpOAuthConnection agent={agent} canEdit name="gateway" />, { wrapper: Shell });

    expect(await screen.findByText("Needs reconnecting")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reconnect gateway" })).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Disconnect gateway" }));
    await waitFor(() => expect(mockDisconnectAgent).toHaveBeenCalledWith("gateway"));
  });

  it("says a workspace entry's connection is the owner's, shared by their agents", async () => {
    lists.agent.servers = [
      agentEntry({ name: "shared-gw", source: "workspace", workspace_server_id: "srv-1", shared: true, connection: connected }),
      agentEntry({ name: "shared-gw", source: "agent" }),
    ];
    render(<AgentMcpOAuthConnection agent={agent} canEdit serverId="srv-1" />, { wrapper: Shell });

    expect(await screen.findByText("Connected")).toBeTruthy();
    expect(screen.getByText("The owner's connection, shared by all of their agents")).toBeTruthy();
  });

  it("gives a viewer who is not the owner the state only", async () => {
    lists.agent = { servers: [agentEntry({})], can_connect: false, available: true };
    render(
      <>
        <AgentMcpOAuthConnection agent={agent} canEdit name="gateway" />
        <AgentMcpOAuthOwnerNote agent={agent} canEdit />
      </>,
      { wrapper: Shell },
    );

    expect(await screen.findByText("Not connected")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("Only the agent's owner can connect it.")).toBeTruthy();
  });

  it("shows an invalid entry as such, with nothing to press", async () => {
    lists.agent.servers = [agentEntry({ problem: "invalid MCP OAuth server: url must be HTTPS" })];
    render(<AgentMcpOAuthConnection agent={agent} canEdit name="gateway" />, { wrapper: Shell });

    expect(await screen.findByText("Invalid OAuth settings")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("renders nothing for an entry without OAuth, or while the feature is off", async () => {
    lists.agent.servers = [agentEntry({ name: "other" })];
    const { container, unmount } = render(<AgentMcpOAuthConnection agent={agent} canEdit name="plain" />, {
      wrapper: Shell,
    });
    await waitFor(() => expect(container.textContent).toBe(""));
    unmount();

    lists.agent = { servers: [agentEntry({})], can_connect: true, available: false };
    const off = render(<AgentMcpOAuthConnection agent={agent} canEdit name="gateway" />, { wrapper: Shell });
    await waitFor(() => expect(off.container.textContent).toBe(""));
  });
});

describe("WorkspaceMcpOAuthConnection", () => {
  it("connects and disconnects the viewer's own connection", async () => {
    lists.workspace.servers = [
      { server_id: "srv-1", name: "gateway", scope: "", problem: "", connection: null },
      { server_id: "srv-2", name: "live", scope: "", problem: "", connection: connected },
    ];
    mockConnectWorkspace.mockResolvedValue("https://auth.example.com/authorize?state=y");
    mockDisconnectWorkspace.mockResolvedValue(undefined);
    render(
      <>
        <WorkspaceMcpOAuthConnection wsId="ws-1" serverId="srv-1" />
        <WorkspaceMcpOAuthConnection wsId="ws-1" serverId="srv-2" />
      </>,
      { wrapper: Shell },
    );

    expect(await screen.findByText("Not connected")).toBeTruthy();
    expect(screen.getAllByText("Your connection, shared by all of your agents")).toHaveLength(2);
    await userEvent.click(screen.getByRole("button", { name: "Connect gateway" }));
    await waitFor(() => expect(mockConnectWorkspace).toHaveBeenCalledWith({ serverId: "srv-1", returnTo: "/acme/agents/agent-1?view=mcp_config" }));
    await userEvent.click(screen.getByRole("button", { name: "Disconnect live" }));
    await waitFor(() => expect(mockDisconnectWorkspace).toHaveBeenCalledWith("srv-2"));
  });
});
