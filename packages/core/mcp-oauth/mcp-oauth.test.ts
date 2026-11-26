// @vitest-environment node
// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: pre-registered OAuth clients (M-295).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiClient, setApiInstance } from "../api";
import {
  completeMcpOAuth,
  connectAgentMcpOAuth,
  connectWorkspaceMcpOAuth,
  deleteAgentMcpOAuthClientSecret,
  deleteWorkspaceMcpOAuthClientSecret,
  disconnectAgentMcpOAuth,
  disconnectWorkspaceMcpOAuth,
  listAgentMcpOAuth,
  listWorkspaceMcpOAuth,
  mcpOAuthConnectionState,
  setAgentMcpOAuthClientSecret,
  setWorkspaceMcpOAuthClientSecret,
} from ".";

let calls: { method: string; url: string; body: unknown; headers: unknown }[] = [];

function serve(route: (url: string, init: RequestInit) => Response) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      calls.push({
        method: init.method ?? "GET",
        url,
        body: init.body ? JSON.parse(String(init.body)) : undefined,
        headers: init.headers,
      });
      return route(url, init);
    }),
  );
}

beforeEach(() => setApiInstance(new ApiClient("https://api.example.test")));
afterEach(() => vi.unstubAllGlobals());

const AGENT = "https://api.example.test/api/agents/agent-1/mcp-oauth";
const WORKSPACE = "https://api.example.test/api/workspaces/ws-1/mcp-oauth";

describe("listAgentMcpOAuth", () => {
  it("reads 404 as unavailable (mcp_oauth_v1 off)", async () => {
    serve(() => Response.json({ error: "not found" }, { status: 404 }));
    await expect(listAgentMcpOAuth("ws-1", "agent-1")).resolves.toEqual({
      servers: [],
      can_connect: false,
      available: false,
    });
  });

  it("scopes the request to the workspace and parses entries with defaults", async () => {
    serve(() =>
      Response.json({
        servers: [
          {
            name: "gateway",
            source: "agent",
            scope: "mcp",
            shared: false,
            connection: { status: "connected", connected_at: "2026-11-24T00:00:00Z", expires_at: "2026-11-24T01:00:00Z" },
          },
          { name: "shared-gw", source: "workspace", workspace_server_id: "srv-1", shared: true },
          { name: "broken", problem: "invalid MCP OAuth server: url must be HTTPS" },
        ],
        can_connect: true,
      }),
    );
    const list = await listAgentMcpOAuth("ws-1", "agent-1");
    expect(calls[0]).toMatchObject({ method: "GET", url: AGENT, headers: { "X-Workspace-ID": "ws-1" } });
    expect(list.available).toBe(true);
    expect(list.can_connect).toBe(true);
    expect(list.servers[0]).toMatchObject({
      name: "gateway",
      source: "agent",
      workspace_server_id: "",
      problem: "",
      connection: { status: "connected", expires_at: "2026-11-24T01:00:00Z", last_error: "", refreshed_at: null },
    });
    expect(list.servers[1]).toMatchObject({ name: "shared-gw", source: "workspace", workspace_server_id: "srv-1", shared: true, connection: null });
    expect(list.servers[2]).toMatchObject({ name: "broken", source: "agent", shared: false, problem: expect.stringContaining("HTTPS") });
  });

  it("falls back safely on a malformed body", async () => {
    serve(() => Response.json({ nope: true }));
    await expect(listAgentMcpOAuth("ws-1", "agent-1")).resolves.toEqual({
      servers: [],
      can_connect: false,
      available: true,
    });
  });

  it("never surfaces fields the schema does not know, such as a token or a URL", async () => {
    serve(() =>
      Response.json({
        servers: [{ name: "gateway", url: "https://leaked.example", access_token: "leaked", connection: { status: "connected", access_token: "leaked" } }],
        can_connect: false,
      }),
    );
    const list = await listAgentMcpOAuth("ws-1", "agent-1");
    expect(JSON.stringify(list)).not.toContain("leaked");
  });

  it("treats an unknown connection status as needing a reconnect", async () => {
    serve(() => Response.json({ servers: [{ name: "gateway", connection: { status: 42 } }], can_connect: true }));
    const list = await listAgentMcpOAuth("ws-1", "agent-1");
    expect(list.servers[0]?.connection?.status).toBe("needs_reconnect");
    expect(mcpOAuthConnectionState(list.servers[0]?.connection ?? null)).toBe("needs_reconnect");
  });
});

describe("listWorkspaceMcpOAuth", () => {
  it("reads 404 as unavailable", async () => {
    serve(() => Response.json({ error: "not found" }, { status: 404 }));
    await expect(listWorkspaceMcpOAuth("ws-1")).resolves.toEqual({ servers: [], available: false });
  });

  it("parses the caller's own connections", async () => {
    serve(() =>
      Response.json({
        servers: [
          { server_id: "srv-1", name: "gateway", scope: "", connection: { status: "needs_reconnect", last_error: "url_changed" } },
          { server_id: "srv-2", name: "fresh" },
        ],
      }),
    );
    const list = await listWorkspaceMcpOAuth("ws-1");
    expect(calls[0]).toMatchObject({ method: "GET", url: WORKSPACE, headers: { "X-Workspace-ID": "ws-1" } });
    expect(list.servers.map((s) => [s.server_id, mcpOAuthConnectionState(s.connection)])).toEqual([
      ["srv-1", "needs_reconnect"],
      ["srv-2", "not_connected"],
    ]);
  });
});

describe("connects and disconnects", () => {
  it("connects an agent entry by name and returns the authorization URL", async () => {
    serve(() => Response.json({ authorization_url: "https://auth.example.com/authorize?x=1" }));
    await expect(connectAgentMcpOAuth("ws-1", "agent-1", "gate way", "/acme/agents/agent-1?view=mcp_config")).resolves.toBe(
      "https://auth.example.com/authorize?x=1",
    );
    expect(calls[0]).toMatchObject({
      method: "POST",
      url: `${AGENT}/gate%20way/connect`,
      body: { return_to: "/acme/agents/agent-1?view=mcp_config" },
    });
  });

  it("connects a workspace entry by id", async () => {
    serve(() => Response.json({ authorization_url: "https://auth.example.com/authorize?y=1" }));
    await expect(connectWorkspaceMcpOAuth("ws-1", "srv-1", "/acme/settings?tab=mcp")).resolves.toBe(
      "https://auth.example.com/authorize?y=1",
    );
    expect(calls[0]).toMatchObject({ method: "POST", url: `${WORKSPACE}/srv-1/connect`, body: { return_to: "/acme/settings?tab=mcp" } });
  });

  it("refuses a connect answer without a URL", async () => {
    serve(() => Response.json({}));
    await expect(connectAgentMcpOAuth("ws-1", "agent-1", "gateway", "/")).rejects.toThrow(/unexpected response/);
  });

  it("disconnects at their paths", async () => {
    serve(() => new Response(null, { status: 204 }));
    await disconnectAgentMcpOAuth("ws-1", "agent-1", "gateway");
    await disconnectWorkspaceMcpOAuth("ws-1", "srv-1");
    expect(calls.map((c) => [c.method, c.url])).toEqual([
      ["DELETE", `${AGENT}/gateway/connection`],
      ["DELETE", `${WORKSPACE}/srv-1/connection`],
    ]);
  });

  it("completes a connect with the state and code", async () => {
    serve(() =>
      Response.json({ workspace_id: "ws-1", agent_id: "", server_id: "srv-1", server_ref: "ws:srv-1", return_to: "/acme/settings?tab=mcp" }),
    );
    await expect(completeMcpOAuth({ state: "st", code: "cd" })).resolves.toEqual({
      workspace_id: "ws-1",
      agent_id: "",
      server_id: "srv-1",
      server_ref: "ws:srv-1",
      return_to: "/acme/settings?tab=mcp",
    });
    expect(calls[0]).toMatchObject({
      method: "POST",
      url: "https://api.example.test/api/mcp-oauth/complete",
      body: { state: "st", code: "cd", error: "" },
    });
  });
});

describe("pre-registered clients", () => {
  it("reads whether an entry names a pre-registered client and has a secret, false when absent", async () => {
    serve(() =>
      Response.json({
        servers: [
          { name: "pre", preregistered: true, client_secret_set: true, client_secret: "leaked" },
          { name: "old-server" },
          { name: "odd", preregistered: "yes", client_secret_set: 1 },
        ],
        can_connect: true,
      }),
    );
    const list = await listAgentMcpOAuth("ws-1", "agent-1");
    expect(list.servers.map((s) => [s.name, s.preregistered, s.client_secret_set])).toEqual([
      ["pre", true, true],
      ["old-server", false, false],
      ["odd", false, false],
    ]);
    expect(JSON.stringify(list)).not.toContain("leaked");

    serve(() => Response.json({ servers: [{ server_id: "srv-1", name: "pre", preregistered: true, client_secret_set: false }] }));
    const ws = await listWorkspaceMcpOAuth("ws-1");
    expect(ws.servers[0]).toMatchObject({ preregistered: true, client_secret_set: false });
  });

  it("writes and deletes the client secret at its own paths", async () => {
    serve(() => new Response(null, { status: 204 }));
    await setAgentMcpOAuthClientSecret("ws-1", "agent-1", "gate way", "sk-1");
    await deleteAgentMcpOAuthClientSecret("ws-1", "agent-1", "gate way");
    await setWorkspaceMcpOAuthClientSecret("ws-1", "srv-1", "sk-2");
    await deleteWorkspaceMcpOAuthClientSecret("ws-1", "srv-1");
    expect(calls.map((c) => [c.method, c.url, c.body])).toEqual([
      ["PUT", `${AGENT}/gate%20way/client-secret`, { client_secret: "sk-1" }],
      ["DELETE", `${AGENT}/gate%20way/client-secret`, undefined],
      ["PUT", `${WORKSPACE}/srv-1/client-secret`, { client_secret: "sk-2" }],
      ["DELETE", `${WORKSPACE}/srv-1/client-secret`, undefined],
    ]);
    expect(calls[0]?.headers).toMatchObject({ "X-Workspace-ID": "ws-1" });
  });
});
