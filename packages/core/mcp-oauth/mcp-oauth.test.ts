// @vitest-environment node
// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiClient, setApiInstance } from "../api";
import {
  completeMcpOAuth,
  connectMcpOAuthServer,
  createMcpOAuthServer,
  deleteMcpOAuthServer,
  disconnectMcpOAuthServer,
  listMcpOAuthServers,
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

const BASE = "https://api.example.test/api/agents/agent-1/mcp-oauth";

describe("listMcpOAuthServers", () => {
  it("reads 404 as unavailable (mcp_oauth_v1 off)", async () => {
    serve(() => Response.json({ error: "not found" }, { status: 404 }));
    await expect(listMcpOAuthServers("ws-1", "agent-1")).resolves.toEqual({
      servers: [],
      can_connect: false,
      available: false,
    });
  });

  it("scopes the request to the workspace and parses servers with defaults", async () => {
    serve(() =>
      Response.json({
        servers: [
          {
            id: "s1",
            name: "gateway",
            url: "https://mcp.example.com/mcp",
            has_extra_headers: true,
            connection: { status: "connected", connected_at: "2026-11-24T00:00:00Z", expires_at: "2026-11-24T01:00:00Z" },
          },
          { id: "s2", name: "pending" },
        ],
        can_connect: true,
      }),
    );
    const list = await listMcpOAuthServers("ws-1", "agent-1");
    expect(calls[0]).toMatchObject({ method: "GET", url: BASE, headers: { "X-Workspace-ID": "ws-1" } });
    expect(list.available).toBe(true);
    expect(list.can_connect).toBe(true);
    expect(list.servers[0]).toMatchObject({
      name: "gateway",
      has_extra_headers: true,
      connection: { status: "connected", expires_at: "2026-11-24T01:00:00Z", last_error: "", refreshed_at: null },
    });
    expect(list.servers[1]).toMatchObject({ name: "pending", url: "", connection: null, has_extra_headers: false });
  });

  it("falls back safely on a malformed body", async () => {
    serve(() => Response.json({ nope: true }));
    await expect(listMcpOAuthServers("ws-1", "agent-1")).resolves.toEqual({
      servers: [],
      can_connect: false,
      available: true,
    });
  });

  it("never surfaces fields the schema does not know, such as a token", async () => {
    serve(() =>
      Response.json({
        servers: [{ id: "s1", name: "gateway", access_token: "leaked", connection: { status: "connected", access_token: "leaked" } }],
        can_connect: false,
      }),
    );
    const list = await listMcpOAuthServers("ws-1", "agent-1");
    expect(JSON.stringify(list)).not.toContain("leaked");
  });

  it("treats an unknown connection status as needing a reconnect", async () => {
    serve(() => Response.json({ servers: [{ id: "s1", connection: { status: 42 } }], can_connect: true }));
    const list = await listMcpOAuthServers("ws-1", "agent-1");
    expect(list.servers[0]?.connection?.status).toBe("needs_reconnect");
  });
});

describe("writes", () => {
  it("creates a server with its headers", async () => {
    serve(() => Response.json({ id: "s1", name: "gateway", url: "https://mcp.example.com/mcp" }, { status: 201 }));
    const server = await createMcpOAuthServer("ws-1", "agent-1", {
      name: "gateway",
      url: "https://mcp.example.com/mcp",
      extraHeaders: { "x-mcp-servers": "browser" },
    });
    expect(server.id).toBe("s1");
    expect(calls[0]).toMatchObject({
      method: "POST",
      url: BASE,
      body: { name: "gateway", url: "https://mcp.example.com/mcp", scope: "", extra_headers: { "x-mcp-servers": "browser" } },
    });
  });

  it("starts a connect and returns the authorization URL", async () => {
    serve(() => Response.json({ authorization_url: "https://auth.example.com/authorize?x=1" }));
    await expect(connectMcpOAuthServer("ws-1", "agent-1", "s1", "/acme/agents/agent-1?view=mcp_oauth")).resolves.toBe(
      "https://auth.example.com/authorize?x=1",
    );
    expect(calls[0]).toMatchObject({
      method: "POST",
      url: `${BASE}/s1/connect`,
      body: { return_to: "/acme/agents/agent-1?view=mcp_oauth" },
    });
  });

  it("refuses a connect answer without a URL", async () => {
    serve(() => Response.json({}));
    await expect(connectMcpOAuthServer("ws-1", "agent-1", "s1", "/")).rejects.toThrow(/unexpected response/);
  });

  it("deletes and disconnects at their paths", async () => {
    serve(() => new Response(null, { status: 204 }));
    await deleteMcpOAuthServer("ws-1", "agent-1", "s1");
    await disconnectMcpOAuthServer("ws-1", "agent-1", "s1");
    expect(calls.map((c) => [c.method, c.url])).toEqual([
      ["DELETE", `${BASE}/s1`],
      ["DELETE", `${BASE}/s1/connection`],
    ]);
  });

  it("completes a connect with the state and code", async () => {
    serve(() => Response.json({ workspace_id: "ws-1", agent_id: "agent-1", server_id: "s1", return_to: "/acme/agents/agent-1" }));
    await expect(completeMcpOAuth({ state: "st", code: "cd" })).resolves.toEqual({
      workspace_id: "ws-1",
      agent_id: "agent-1",
      server_id: "s1",
      return_to: "/acme/agents/agent-1",
    });
    expect(calls[0]).toMatchObject({
      method: "POST",
      url: "https://api.example.test/api/mcp-oauth/complete",
      body: { state: "st", code: "cd", error: "" },
    });
  });
});
