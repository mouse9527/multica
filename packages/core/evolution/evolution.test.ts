// @vitest-environment node
// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiClient, setApiInstance } from "../api";
import { enableEvolution, getEvolution } from ".";

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

describe("getEvolution", () => {
  it("reads 404 as unavailable (evolution_v1 off)", async () => {
    serve(() => Response.json({ error: "not found" }, { status: 404 }));
    await expect(getEvolution("ws-1")).resolves.toEqual({ available: false, agent: null });
  });

  it("reads a null agent as available but not enabled", async () => {
    serve(() => Response.json({ agent: null }));
    await expect(getEvolution("ws-1")).resolves.toEqual({ available: true, agent: null });
    expect(calls[0]).toMatchObject({ url: "https://api.example.test/api/evolution", headers: { "X-Workspace-ID": "ws-1" } });
  });

  it("parses the agent with defaults", async () => {
    serve(() => Response.json({ agent: { id: "a1", name: "进化" } }));
    await expect(getEvolution("ws")).resolves.toEqual({
      available: true,
      agent: { id: "a1", name: "进化", avatar_url: null, runtime_id: null, model: null, status: "" },
    });
  });

  it.each([
    ["a null body", null],
    ["a wrong shape", { agent: "nope" }],
  ])("reads %s as not enabled", async (_name, body) => {
    serve(() => Response.json(body));
    await expect(getEvolution("ws")).resolves.toEqual({ available: true, agent: null });
  });

  it("rethrows other errors", async () => {
    serve(() => Response.json({ error: "boom" }, { status: 500 }));
    await expect(getEvolution("ws")).rejects.toThrow();
  });
});

describe("enableEvolution", () => {
  it("posts runtime, language and model", async () => {
    serve(() => Response.json({ agent: { id: "a1", name: "Evolution" } }, { status: 201 }));
    const agent = await enableEvolution("ws-1", { runtimeId: "rt", language: "en" });
    expect(agent.id).toBe("a1");
    expect(calls[0]).toMatchObject({
      method: "POST",
      url: "https://api.example.test/api/evolution/enable",
      body: { runtime_id: "rt", language: "en", model: "" },
    });
  });

  it("fails on a response without an agent", async () => {
    serve(() => Response.json({ agent: null }));
    await expect(enableEvolution("ws", { runtimeId: "rt", language: "en" })).rejects.toThrow(/unexpected response/);
  });
});
