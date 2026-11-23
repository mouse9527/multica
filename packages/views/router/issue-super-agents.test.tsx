// Added by the mouse9527/multica fork: which Super Agents cover an issue (M-194).
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../test/i18n";
import en from "../locales/en/router.json";
import { IssueSuperAgentsRow } from "./issue-super-agents";

const state = vi.hoisted(() => ({ flag: true, covering: [] as unknown[] }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => state.flag }));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: { request: vi.fn() },
}));

const request = vi.mocked(api.request);

function covering(over: Record<string, unknown> = {}) {
  return {
    subscription_id: "sub-tree",
    agent_id: "agent-1",
    agent_name: "Mika",
    owner_user_id: "user-1",
    scope_type: "issue_tree",
    scope_id: "issue-root",
    scope_name: "M-45",
    chat_bound: true,
    chat_session_id: "chat-1",
    chat_title: "Feature lead",
    enabled: true,
    paused_reason: null,
    effective: true,
    superseded_by: null,
    ...over,
  };
}

function show() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <div className="grid grid-cols-[auto_1fr]">
        <IssueSuperAgentsRow issueId="issue-9" />
      </div>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  state.flag = true;
  state.covering = [];
  request.mockImplementation(async () => ({ issue_id: "issue-9", covering: state.covering }));
});

describe("IssueSuperAgentsRow", () => {
  it("shows nothing and asks nothing while router_chat is off", () => {
    state.flag = false;
    show();
    expect(screen.queryByText(en.covering.label)).not.toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
  });

  it("says when no Super Agent covers the issue", async () => {
    show();
    expect(await screen.findByText(en.covering.none)).toBeInTheDocument();
    expect(request.mock.calls[0]?.[0]).toBe("/api/router/issues/issue-9/covering");
  });

  it("lists each covering Super Agent with its scope, chat and state", async () => {
    state.covering = [
      covering(),
      covering({ subscription_id: "sub-ops", agent_name: "ops", scope_type: "project", scope_name: "home", chat_session_id: null, chat_title: null, paused_reason: "agent_unavailable" }),
      covering({ subscription_id: "sub-wide", scope_type: "project", scope_name: "multica", effective: false, superseded_by: "sub-tree", chat_title: "" }),
    ];
    show();
    const items = await screen.findAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(items[0]).toHaveTextContent("Mika");
    expect(items[0]).toHaveTextContent("M-45");
    expect(items[0]).toHaveTextContent("Feature lead");
    expect(within(items[1]!).getByText(en.status.reason.agent_unavailable)).toBeInTheDocument();
    expect(items[1]).not.toHaveTextContent("·");
    await waitFor(() => expect(items[2]).toHaveTextContent(en.covering.superseded));
    expect(items[2]).toHaveTextContent(en.list.untitled);
  });
});
