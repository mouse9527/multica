// Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96).
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatMessage } from "@multica/core/types";
import type { ReactElement } from "react";
import enChat from "../../locales/en/chat.json";

// Same stub as chat-message-list.test.tsx: real react-virtuoso renders no
// data rows under jsdom's zero-height viewport.
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({
    data,
    itemContent,
    computeItemKey,
  }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}));

import { ChatMessageList } from "./chat-message-list";
import { hideSilentRouterTurns, routerBatchHeadline } from "./router-batch";

const BATCH =
  "[Router 事件批次 · 非 Owner 发言]\nRouter batch for subscription s1 (issue tree RT-1, anchor RT-1): 2 new event(s).\nEvents, oldest first:\n- RT-2 comment_created";

function msg(over: Partial<ChatMessage> & Pick<ChatMessage, "id">): ChatMessage {
  return {
    chat_session_id: "session-1",
    role: "user",
    content: "",
    task_id: null,
    created_at: "2026-11-16T00:00:00Z",
    message_kind: "message",
    ...over,
  } as ChatMessage;
}

describe("hideSilentRouterTurns", () => {
  it("drops the no_response of the turn that owns a batch and keeps every other row", () => {
    const messages = [
      msg({ id: "b1", content: BATCH, task_id: "t1", message_kind: "router_batch" }),
      // The member spoke while the batch turn was queued; their turn's
      // reply lands between the batch and the batch's own reply.
      msg({ id: "u1", content: "status?", task_id: "t2" }),
      msg({ id: "a2", role: "assistant", task_id: "t2", message_kind: "no_response" }),
      msg({ id: "a1", role: "assistant", task_id: "t1", message_kind: "no_response" }),
      msg({ id: "b2", content: BATCH, task_id: "t3", message_kind: "router_batch" }),
      msg({ id: "f3", role: "assistant", content: "failed", task_id: "t3" }),
      // Right after a batch, but owned by another turn: kept.
      msg({ id: "b4", content: BATCH, task_id: "t4", message_kind: "router_batch" }),
      msg({ id: "a5", role: "assistant", task_id: "t5", message_kind: "no_response" }),
    ];
    expect(hideSilentRouterTurns(messages).map((m) => m.id)).toEqual(["b1", "u1", "a2", "b2", "f3", "b4", "a5"]);
  });

  it("returns the list untouched when the chat has no batch", () => {
    const messages = [msg({ id: "a1", role: "assistant", message_kind: "no_response" })];
    expect(hideSilentRouterTurns(messages)).toBe(messages);
  });

  it("reads the label and summary from the batch's first two lines", () => {
    expect(routerBatchHeadline(BATCH)).toEqual({
      label: "[Router 事件批次 · 非 Owner 发言]",
      summary: "Router batch for subscription s1 (issue tree RT-1, anchor RT-1): 2 new event(s).",
    });
  });
});

describe("ChatMessageList Router batches", () => {
  it("shows a batch collapsed, hides its silent turn, and keeps the reply to the next one", () => {
    render(
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              msg({ id: "b1", content: BATCH, task_id: "t1", message_kind: "router_batch" }),
              msg({ id: "a1", role: "assistant", content: "no reply", task_id: "t1", message_kind: "no_response" }),
              msg({ id: "b2", content: BATCH, task_id: "t2", message_kind: "router_batch" }),
              msg({ id: "a2", role: "assistant", content: "RT-2 needs your decision", task_id: "t2" }),
            ]}
            pendingTask={null}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(screen.getAllByText("[Router 事件批次 · 非 Owner 发言]")).toHaveLength(2);
    expect(screen.queryByText(/RT-2 comment_created/)).not.toBeInTheDocument();
    expect(screen.queryByText(enChat.message_list.no_response)).not.toBeInTheDocument();
    expect(screen.getByText("RT-2 needs your decision")).toBeInTheDocument();

    fireEvent.click(screen.getAllByText("[Router 事件批次 · 非 Owner 发言]")[0]!);
    expect(screen.getByText(/RT-2 comment_created/)).toBeInTheDocument();
  });
});
