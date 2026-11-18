/** @vitest-environment jsdom */
// Added by the mouse9527/multica fork: live Super Agent settings (M-147).
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { WSClient } from "../api/ws-client";
import { createQueryClient } from "../query-client";
import { routerSubscriptionsOptions } from "../router/queries";
import type { WSMessage } from "../types";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));
vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const subscription = {
  id: "sub-1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  owner_user_id: "user-1",
  scope_type: "issue_tree",
  scope_id: "issue-1",
  anchor_issue_id: "issue-1",
  anchor_chat_session_id: "chat-1",
  event_types: ["comment_created"],
  enabled: true,
  paused_reason: null,
  debounce_seconds: 60,
  max_wait_seconds: 600,
  max_runs_per_hour: 20,
  reconciled_through: "2026-11-18T00:00:00Z",
  created_by_type: "agent",
  created_by_id: "agent-1",
  created_at: "2026-11-18T00:00:00Z",
  event_count: 0,
  capture_miss: 0,
};

it("refetches the Super Agent subscriptions when one changes elsewhere (M-147)", async () => {
  const qc = createQueryClient();
  let rows: unknown[] = [];
  const request = vi.fn(async () => ({ subscriptions: rows }));
  setApiInstance({ request } as unknown as ApiClient);

  let emit: ((msg: WSMessage) => void) | undefined;
  const ws = {
    on: () => () => {},
    onAny: (handler: (msg: WSMessage) => void) => {
      emit = handler;
      return () => {
        emit = undefined;
      };
    },
    onReconnect: () => () => {},
  } as unknown as WSClient;
  const stores = {
    authStore: { getState: () => ({ user: { id: "user-1" } }) },
  } as unknown as RealtimeSyncStores;
  function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  }

  const view = renderHook(
    () => {
      useRealtimeSync(ws, stores);
      return useQuery(routerSubscriptionsOptions("ws-1"));
    },
    { wrapper: Wrapper },
  );
  try {
    await waitFor(() => expect(view.result.current.data).toEqual([]));

    // The Super Agent subscribes from its chat run through the CLI.
    rows = [subscription];
    act(() => {
      emit!({ type: "router:subscription_changed", payload: { subscription_id: "sub-1", action: "created" } } as unknown as WSMessage);
    });
    await waitFor(() => expect(view.result.current.data?.map((s) => s.id)).toEqual(["sub-1"]));
    expect(request).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    qc.clear();
  }
});
