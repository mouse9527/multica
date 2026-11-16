// @vitest-environment node
// Added by the mouse9527/multica fork: Router page settings (M-105).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiClient, setApiInstance } from "../api";
import {
  addRouterScopes,
  groupRouters,
  latestDispatch,
  listRouterDispatches,
  listRouterSubscriptions,
  reconciledThrough,
  releaseRouter,
  routerStatus,
  setRouterPaused,
  type RouterDispatch,
  type RouterSubscription,
} from ".";

function sub(over: Partial<RouterSubscription> = {}): RouterSubscription {
  return {
    id: "s1",
    workspace_id: "ws",
    agent_id: "a1",
    owner_user_id: "u1",
    scope_type: "issue_tree",
    scope_id: "i1",
    anchor_issue_id: "i1",
    anchor_chat_session_id: "c1",
    event_types: [],
    enabled: true,
    paused_reason: null,
    debounce_seconds: 60,
    max_wait_seconds: 600,
    max_runs_per_hour: 6,
    reconciled_through: "2026-11-16T10:00:00Z",
    created_by_type: "member",
    created_by_id: "u1",
    created_at: "2026-11-16T09:00:00Z",
    event_count: 0,
    capture_miss: 0,
    ...over,
  };
}

const BOARD = {
  id: "board-1",
  workspace_id: "ws",
  number: 9,
  identifier: "M-9",
  title: "Router board",
  description: null,
  status: "backlog",
  priority: "none",
  assignee_type: null,
  assignee_id: null,
  creator_type: "member",
  creator_id: "u1",
  parent_issue_id: null,
  project_id: "proj",
  position: 0,
  stage: null,
  start_date: null,
  due_date: null,
  metadata: {},
  properties: {},
  created_at: "2026-11-16T00:00:00Z",
  updated_at: "2026-11-16T00:00:00Z",
};

const SETTINGS = { event_types: ["issue_created"], debounce_seconds: 60, max_wait_seconds: 600, max_runs_per_hour: 6 };

type Route = (url: string, init: RequestInit) => Response;
let calls: { method: string; url: string; body: unknown }[] = [];

function serve(route: Route) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      calls.push({ method, url, body: init.body ? JSON.parse(String(init.body)) : undefined });
      return route(url, init);
    }),
  );
}

beforeEach(() => setApiInstance(new ApiClient("https://api.example.test")));
afterEach(() => vi.unstubAllGlobals());

describe("router reads", () => {
  it("sends the workspace id and parses subscriptions with defaults", async () => {
    serve(() => Response.json({ subscriptions: [{ id: "s", agent_id: "a", scope_type: "project", scope_id: "p" }] }));
    const subs = await listRouterSubscriptions("ws-1");
    expect(subs).toEqual([
      expect.objectContaining({ id: "s", anchor_chat_session_id: null, enabled: true, event_types: [], capture_miss: 0 }),
    ]);
    const init = vi.mocked(fetch).mock.calls[0]?.[1] as RequestInit;
    expect(init.headers).toMatchObject({ "X-Workspace-ID": "ws-1" });
  });

  it.each([
    ["a null body", null],
    ["a wrong shape", { subscriptions: "nope" }],
  ])("reads %s as no subscriptions", async (_name, body) => {
    serve(() => Response.json(body));
    await expect(listRouterSubscriptions("ws")).resolves.toEqual([]);
  });

  it("reads a malformed dispatch list as empty", async () => {
    serve(() => Response.json({ dispatches: [{ id: 1 }] }));
    await expect(listRouterDispatches("ws")).resolves.toEqual([]);
  });
});

describe("grouping and status", () => {
  it("groups by own chat and drops unbound subscriptions and other members' chats", () => {
    const routers = groupRouters(
      [
        sub({ id: "a", anchor_chat_session_id: "c1" }),
        sub({ id: "b", anchor_chat_session_id: null }),
        sub({ id: "c", anchor_chat_session_id: "c2" }),
        sub({ id: "d", anchor_chat_session_id: "c1" }),
      ],
      new Set(["c1"]),
    );
    expect(routers).toEqual([{ chat_session_id: "c1", agent_id: "a1", subscriptions: [expect.objectContaining({ id: "a" }), expect.objectContaining({ id: "d" })] }]);
  });

  it("reports every distinct pause reason", () => {
    expect(routerStatus([sub()])).toEqual({ kind: "running" });
    expect(
      routerStatus([
        sub({ enabled: false, paused_reason: "manual" }),
        sub({ paused_reason: "rate" }),
        sub({ enabled: false, paused_reason: null }),
      ]),
    ).toEqual({ kind: "paused", reasons: ["manual", "rate"] });
  });

  it("picks the newest dispatch of the Router's subscriptions", () => {
    const d = (id: string, subscription_id: string, created_at: string) =>
      ({ id, subscription_id, created_at }) as RouterDispatch;
    expect(
      latestDispatch([d("x", "s1", "2026-11-16T01:00:00Z"), d("y", "other", "2026-11-16T03:00:00Z"), d("z", "s1", "2026-11-16T02:00:00Z")], [sub()]),
    ).toMatchObject({ id: "z" });
    expect(latestDispatch([], [sub()])).toBeNull();
  });

  it("verifies capture only up to the oldest reconcile bound", () => {
    expect(reconciledThrough([sub({ reconciled_through: "2026-11-16T02:00:00Z" }), sub({ reconciled_through: "2026-11-16T01:00:00Z" })])).toBe(
      "2026-11-16T01:00:00Z",
    );
    expect(reconciledThrough([sub(), sub({ reconciled_through: null })])).toBeNull();
  });
});

describe("addRouterScopes", () => {
  it("binds each scope to the chat and gives a project scope its own board issue", async () => {
    serve((url, init) => {
      if (url.endsWith("/api/issues")) return Response.json(BOARD, { status: 201 });
      if (url.endsWith("/api/router/subscriptions") && init.method === "POST") return Response.json({ id: "new", agent_id: "a1", scope_type: "x", scope_id: "y" }, { status: 201 });
      return new Response(null, { status: 500 });
    });
    const result = await addRouterScopes("ws", {
      chatSessionId: "chat-1",
      agentId: "a1",
      scopes: [
        { scope_type: "issue_tree", scope_id: "root" },
        { scope_type: "project", scope_id: "proj" },
      ],
      settings: SETTINGS,
      board: { title: "Router board", description: "d" },
    });
    expect(result.failed).toEqual([]);
    expect(result.created).toHaveLength(2);
    expect(result.boards.map((b) => b.id)).toEqual(["board-1"]);
    const posts = calls.filter((c) => c.method === "POST");
    expect(posts.map((c) => new URL(c.url).pathname)).toEqual([
      "/api/router/subscriptions",
      "/api/issues",
      "/api/router/subscriptions",
    ]);
    expect(posts[0]?.body).toMatchObject({ scope_type: "issue_tree", scope_id: "root", anchor_chat_session_id: "chat-1", agent_id: "a1" });
    expect(posts[0]?.body).not.toHaveProperty("anchor_issue_id");
    expect(posts[1]?.body).toMatchObject({ project_id: "proj", title: "Router board" });
    expect(posts[2]?.body).toMatchObject({ scope_type: "project", anchor_issue_id: "board-1", anchor_chat_session_id: "chat-1", ...SETTINGS });
  });

  it("deletes the board issue again when its subscription is refused", async () => {
    serve((url, init) => {
      if (url.endsWith("/api/issues")) return Response.json(BOARD, { status: 201 });
      if (url.endsWith("/api/issues/board-1") && init.method === "DELETE") return new Response(null, { status: 204 });
      return Response.json({ error: "subscription already exists" }, { status: 409 });
    });
    const result = await addRouterScopes("ws", {
      chatSessionId: "chat-1",
      agentId: "a1",
      scopes: [{ scope_type: "project", scope_id: "proj" }],
      settings: SETTINGS,
      board: { title: "b", description: "d" },
    });
    expect(result.created).toEqual([]);
    expect(result.boards).toEqual([]);
    expect(result.failed).toHaveLength(1);
    expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/api/issues/board-1"))).toBe(true);
  });
});

describe("pause and release", () => {
  const ok = () => serve((url) => (url.includes("/subscriptions/") ? Response.json({ id: "s", agent_id: "a", scope_type: "x", scope_id: "y" }) : new Response(null, { status: 204 })));

  it("pauses running subscriptions and resumes every paused one", async () => {
    ok();
    await setRouterPaused("ws", [sub({ id: "run" }), sub({ id: "off", enabled: false, paused_reason: "manual" })], true);
    expect(calls.map((c) => `${c.method} ${new URL(c.url).pathname}`)).toEqual(["POST /api/router/subscriptions/run/pause"]);
    ok();
    await setRouterPaused(
      "ws",
      [sub({ id: "run" }), sub({ id: "off", enabled: false, paused_reason: "manual" }), sub({ id: "rate", paused_reason: "rate" })],
      false,
    );
    expect(calls.map((c) => new URL(c.url).pathname)).toEqual([
      "/api/router/subscriptions/off/resume",
      "/api/router/subscriptions/rate/resume",
    ]);
  });

  it("keeps subscriptions by pausing them before unbinding", async () => {
    ok();
    await releaseRouter("ws", [sub({ id: "s1" }), sub({ id: "s2", enabled: false, paused_reason: "manual" })], "keep_paused");
    expect(calls.map((c) => `${c.method} ${new URL(c.url).pathname}`)).toEqual([
      "POST /api/router/subscriptions/s1/pause",
      "PATCH /api/router/subscriptions/s1",
      "PATCH /api/router/subscriptions/s2",
    ]);
    expect(calls[1]?.body).toEqual({ anchor_chat_session_id: "none" });
  });

  it("deletes every subscription", async () => {
    ok();
    await releaseRouter("ws", [sub({ id: "s1" }), sub({ id: "s2" })], "delete");
    expect(calls.map((c) => `${c.method} ${new URL(c.url).pathname}`)).toEqual([
      "DELETE /api/router/subscriptions/s1",
      "DELETE /api/router/subscriptions/s2",
    ]);
  });
});
