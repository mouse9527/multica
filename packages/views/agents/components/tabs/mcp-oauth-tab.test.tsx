// @vitest-environment jsdom
// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Agent } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enMcpOAuth from "../../../locales/en/mcpoauth.json";
import { McpOAuthTab, parseExtraHeaders } from "./mcp-oauth-tab";

const list = vi.hoisted(() => ({
  current: { servers: [] as Array<Record<string, unknown>>, can_connect: true, available: true },
}));
const mockCreate = vi.hoisted(() => vi.fn());
const mockDelete = vi.hoisted(() => vi.fn());
const mockDisconnect = vi.hoisted(() => vi.fn());
const mockConnect = vi.hoisted(() => vi.fn());
const mockOpenExternal = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/mcp-oauth", () => ({
  mcpOAuthServersOptions: (wsId: string, agentId: string) => ({
    queryKey: ["mcp-oauth", wsId, "agent", agentId],
    queryFn: () => Promise.resolve(list.current),
  }),
  useCreateMcpOAuthServer: () => ({ mutateAsync: mockCreate, isPending: false }),
  useDeleteMcpOAuthServer: () => ({ mutateAsync: mockDelete, isPending: false }),
  useDisconnectMcpOAuthServer: () => ({ mutateAsync: mockDisconnect, isPending: false }),
  useConnectMcpOAuthServer: () => ({ mutateAsync: mockConnect, isPending: false }),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

vi.mock("../../../navigation", () => ({
  useNavigation: () => ({
    pathname: "/acme/agents/agent-1",
    searchParams: new URLSearchParams("view=mcp_oauth"),
    hash: "",
    replace: vi.fn(),
    push: vi.fn(),
  }),
  currentPath: (nav: { pathname: string; searchParams: URLSearchParams; hash: string }) =>
    `${nav.pathname}?${nav.searchParams.toString()}${nav.hash}`,
}));

vi.mock("../../../platform", () => ({
  openExternal: mockOpenExternal,
  isDesktopShell: () => false,
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), message: vi.fn() },
}));

const agent = { id: "agent-1", name: "Helper" } as Agent;

function Shell({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <I18nProvider locale="en" resources={{ en: { mcpoauth: enMcpOAuth } }}>
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    </I18nProvider>
  );
}

const server = (over: Record<string, unknown>) => ({
  id: "s1",
  name: "gateway",
  url: "https://mcp.example.com/mcp",
  scope: "",
  has_extra_headers: false,
  created_at: "2026-11-24T00:00:00Z",
  connection: null,
  ...over,
});

beforeEach(() => {
  vi.clearAllMocks();
  list.current = { servers: [], can_connect: true, available: true };
});

describe("McpOAuthTab", () => {
  it("shows each server's connection state", async () => {
    list.current.servers = [
      server({ id: "s1", name: "live", connection: { status: "connected", connected_at: "x", expires_at: null, last_error: "", refreshed_at: null } }),
      server({ id: "s2", name: "dead", connection: { status: "needs_reconnect", connected_at: "x", expires_at: null, last_error: "refresh_rejected", refreshed_at: null } }),
      server({ id: "s3", name: "new" }),
    ];
    render(<McpOAuthTab agent={agent} />, { wrapper: Shell });
    const rows = await screen.findAllByRole("listitem");
    expect(within(rows[0]!).getByText("Connected")).toBeTruthy();
    expect(within(rows[0]!).queryByRole("button", { name: "Connect" })).toBeNull();
    expect(within(rows[0]!).getByRole("button", { name: "Disconnect" })).toBeTruthy();
    expect(within(rows[1]!).getByText("Needs reconnecting")).toBeTruthy();
    expect(within(rows[1]!).getByRole("button", { name: "Reconnect" })).toBeTruthy();
    expect(within(rows[2]!).getByText("Not connected")).toBeTruthy();
    expect(within(rows[2]!).getByRole("button", { name: "Connect" })).toBeTruthy();
    expect(within(rows[2]!).queryByRole("button", { name: "Disconnect" })).toBeNull();
  });

  it("opens the authorization URL in this tab and comes back to the same view", async () => {
    list.current.servers = [server({})];
    mockConnect.mockResolvedValue("https://auth.example.com/authorize?state=x");
    render(<McpOAuthTab agent={agent} />, { wrapper: Shell });
    await userEvent.click(await screen.findByRole("button", { name: "Connect" }));
    await waitFor(() =>
      expect(mockOpenExternal).toHaveBeenCalledWith("https://auth.example.com/authorize?state=x", { webTarget: "same-tab" }),
    );
    expect(mockConnect).toHaveBeenCalledWith({ serverId: "s1", returnTo: "/acme/agents/agent-1?view=mcp_oauth" });
  });

  it("offers no connect button to someone who is not the owner, and says why", async () => {
    list.current = { servers: [server({})], can_connect: false, available: true };
    render(<McpOAuthTab agent={agent} />, { wrapper: Shell });
    await screen.findByText("gateway");
    expect(screen.queryByRole("button", { name: "Connect" })).toBeNull();
    expect(screen.getByText("Only the agent's owner can connect it.")).toBeTruthy();
  });

  it("adds a server with its extra headers and keeps the input when the server refuses", async () => {
    mockCreate.mockRejectedValueOnce(new Error("invalid MCP OAuth server: url must be HTTPS"));
    mockCreate.mockResolvedValueOnce(server({ name: "gateway" }));
    render(<McpOAuthTab agent={agent} />, { wrapper: Shell });
    await userEvent.click(await screen.findByRole("button", { name: "Add server" }));
    await userEvent.type(screen.getByLabelText("Name"), "gateway");
    await userEvent.type(screen.getByLabelText("MCP server URL"), "https://mcp.example.com/mcp");
    await userEvent.type(screen.getByLabelText("Extra headers (optional)"), "x-mcp-servers: browser");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("url must be HTTPS");
    expect(screen.getByLabelText("Name")).toHaveValue("gateway");

    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(screen.queryByLabelText("Name")).toBeNull());
    expect(mockCreate).toHaveBeenLastCalledWith({
      name: "gateway",
      url: "https://mcp.example.com/mcp",
      scope: "",
      extraHeaders: { "x-mcp-servers": "browser" },
    });
  });

  it("refuses malformed header lines before sending anything", async () => {
    render(<McpOAuthTab agent={agent} />, { wrapper: Shell });
    await userEvent.click(await screen.findByRole("button", { name: "Add server" }));
    await userEvent.type(screen.getByLabelText("Name"), "gateway");
    await userEvent.type(screen.getByLabelText("MCP server URL"), "https://mcp.example.com/mcp");
    await userEvent.type(screen.getByLabelText("Extra headers (optional)"), "no colon here");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(screen.getByText("Write each header on its own line as Name: value.")).toBeTruthy();
    expect(mockCreate).not.toHaveBeenCalled();
  });
});

describe("parseExtraHeaders", () => {
  it("reads one Name: value per line and skips blank lines", () => {
    expect(parseExtraHeaders("a: 1\n\n  b-c : two words  \n")).toEqual({ a: "1", "b-c": "two words" });
    expect(parseExtraHeaders("")).toEqual({});
  });

  it("refuses a line without a name or value", () => {
    expect(parseExtraHeaders("novalue:")).toBeNull();
    expect(parseExtraHeaders(": nameless")).toBeNull();
    expect(parseExtraHeaders("plain")).toBeNull();
  });
});
