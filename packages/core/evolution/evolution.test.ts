// @vitest-environment node
// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136, M-137, M-138).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiClient, ApiError, setApiInstance } from "../api";
import {
  applyEvolutionProposal,
  enableEvolution,
  getEvolution,
  getEvolutionConfig,
  getEvolutionProposal,
  rollbackEvolutionProposal,
  saveEvolutionConfig,
  startsChatReading,
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

describe("getEvolutionConfig", () => {
  it("parses the state", async () => {
    serve(() =>
      Response.json({
        config: {
          enabled: true,
          schedule: { time: "04:30", timezone: "UTC", interval_days: 2 },
          sources: { comments: true, chat: true },
          exclude_projects: ["p1"],
          exclude_agents: null,
          max_items_per_window: 500,
          max_window_days: 3,
          max_runs_per_day: 2,
        },
        chat_active: true,
        comments_active: true,
        limits: { items_per_window: [50, 5000], window_days: [1, 30], runs_per_day: [1, 10], interval_days: [1, 7], max_excluded: 200 },
        runs: [{ id: "r1", kind: "scheduled", status: "succeeded", issue_id: "i1", created_at: "2026-11-19T03:00:00Z" }],
      }),
    );
    const state = await getEvolutionConfig("ws-1");
    expect(calls[0]).toMatchObject({ url: "https://api.example.test/api/evolution/config", headers: { "X-Workspace-ID": "ws-1" } });
    expect(state.config.schedule).toEqual({ time: "04:30", timezone: "UTC", interval_days: 2 });
    expect(state.config.exclude_agents).toEqual([]);
    // An older server sends no P3 or P5 fields: the defaults fill in.
    expect(state.config).toMatchObject({ max_proposals_per_run: 5, min_evidence_count: 2, rejected_cooldown_days: 30 });
    expect(state.config).toMatchObject({ effect_window_days: 7, regression_threshold_percent: 20, min_effect_sample: 5 });
    expect(state.chat_active).toBe(true);
    expect(state.runs[0]).toMatchObject({ id: "r1", status: "succeeded", finished_at: null, served_items: 0 });
  });

  it.each([
    ["a null body", null],
    ["a wrong shape", { config: "nope", runs: "nope" }],
  ])("falls back to the defaults on %s", async (_name, body) => {
    serve(() => Response.json(body));
    const state = await getEvolutionConfig("ws");
    expect(state.config).toMatchObject({
      enabled: false,
      sources: { comments: true, chat: false },
      schedule: { time: "03:00", timezone: "Asia/Shanghai", interval_days: 1 },
      max_items_per_window: 2000,
    });
    expect(state.chat_active).toBe(false);
    expect(state.runs).toEqual([]);
  });
});

describe("saveEvolutionConfig", () => {
  const config = {
    enabled: true,
    schedule: { time: "03:00", timezone: "Asia/Shanghai", interval_days: 1 },
    sources: { comments: true, chat: true },
    exclude_projects: [],
    exclude_agents: [],
    max_items_per_window: 2000,
    max_window_days: 7,
    max_runs_per_day: 3,
    max_proposals_per_run: 5,
    min_evidence_count: 2,
    rejected_cooldown_days: 30,
    effect_window_days: 14,
    regression_threshold_percent: 30,
    min_effect_sample: 8,
  };

  it("puts the whole config and the member-notice confirmation", async () => {
    serve(() => Response.json({ config, chat_active: true }));
    const state = await saveEvolutionConfig("ws-1", { config, acknowledgeMemberNotice: true });
    expect(state.chat_active).toBe(true);
    expect(calls[0]).toMatchObject({
      method: "PUT",
      url: "https://api.example.test/api/evolution/config",
      body: { config, acknowledge_member_notice: true },
    });
  });

  it("rejects when the server refuses (chat without the confirmation)", async () => {
    serve(() => Response.json({ error: "confirm with acknowledge_member_notice" }, { status: 409 }));
    await expect(saveEvolutionConfig("ws", { config })).rejects.toThrow();
    expect(calls[0]?.body).toMatchObject({ acknowledge_member_notice: false });
  });
});

describe("startsChatReading", () => {
  it("is true only when the change starts reading chats", async () => {
    serve(() => Response.json(null));
    const current = await getEvolutionConfig("ws");
    const on = { ...current.config, enabled: true, sources: { comments: true, chat: true } };
    expect(startsChatReading(current, on)).toBe(true);
    expect(startsChatReading({ ...current, chat_active: true }, on)).toBe(false);
    expect(startsChatReading(current, { ...on, enabled: false })).toBe(false);
    expect(startsChatReading(current, { ...on, sources: { comments: true, chat: false } })).toBe(false);
  });
});

describe("getEvolutionProposal", () => {
  it("reads 404 as not an applicable proposal", async () => {
    serve(() => Response.json({ error: "not found" }, { status: 404 }));
    await expect(getEvolutionProposal("ws-1", "i1")).resolves.toBeNull();
    expect(calls[0]).toMatchObject({
      url: "https://api.example.test/api/evolution/proposals/i1",
      headers: { "X-Workspace-ID": "ws-1" },
    });
  });

  it("parses the state with defaults", async () => {
    serve(() =>
      Response.json({
        issue_id: "i1",
        target_type: "agent",
        target_name: "Reviewer",
        field: "instructions",
        change: { before: "old", after: "new" },
        state: "applied",
        applicable: true,
        can_rollback: true,
        versions: [{ id: "v1", version: 1, action: "apply", field: "instructions", created_at: "2026-11-23T08:00:00Z" }],
      }),
    );
    const state = await getEvolutionProposal("ws", "i1");
    expect(state).toMatchObject({
      state: "applied",
      can_apply: false,
      can_rollback: true,
      stale: false,
      changed_since_apply: false,
      change: { before: "old", after: "new" },
    });
    expect(state?.versions[0]).toMatchObject({ id: "v1", reverted_of: null, actor_id: "" });
  });

  it("offers nothing on an unknown state or a malformed body", async () => {
    serve(() => Response.json({ issue_id: "i1", state: "exploded", can_apply: "yes", versions: "nope" }));
    await expect(getEvolutionProposal("ws", "i1")).resolves.toMatchObject({
      state: "pending",
      can_apply: false,
      can_rollback: false,
      versions: [],
    });
    serve(() => Response.json(null));
    await expect(getEvolutionProposal("ws", "i1")).resolves.toBeNull();
  });
});

describe("applyEvolutionProposal and rollbackEvolutionProposal", () => {
  const applied = { issue_id: "i1", state: "applied", can_rollback: true };

  it("post to the proposal's routes", async () => {
    serve(() => Response.json(applied));
    await expect(applyEvolutionProposal("ws-1", "i1")).resolves.toMatchObject({ state: "applied" });
    await rollbackEvolutionProposal("ws-1", "i1");
    expect(calls.map((c) => [c.method, c.url])).toEqual([
      ["POST", "https://api.example.test/api/evolution/proposals/i1/apply"],
      ["POST", "https://api.example.test/api/evolution/proposals/i1/rollback"],
    ]);
  });

  it("rejects with the server's reason when refused", async () => {
    serve(() => Response.json({ code: "proposal_stale", error: "the agent changed" }, { status: 409 }));
    const err = await applyEvolutionProposal("ws", "i1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
  });

  it("fails on a response it cannot read", async () => {
    serve(() => Response.json({ state: "applied" }));
    await expect(applyEvolutionProposal("ws", "i1")).rejects.toThrow(/unexpected response/);
  });
});
