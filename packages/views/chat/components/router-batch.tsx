// Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96).
//
// A Router chat receives batches of issue events the server writes in as
// `router_batch` messages. They are input for the agent, not something the
// member said: they render as a collapsed card, and a batch turn that ended
// without a reply (`no_response`) is not shown at all — staying quiet is the
// normal outcome for a batch that needs nothing from the member.

import { useState } from "react";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@multica/ui/components/ui/collapsible";
import { ChevronDown, ChevronRight } from "@multica/ui/icons";
import type { ChatMessage } from "@multica/core/types";
import { RichContent } from "../../rich-content";

/**
 * A Router batch, kind `router_batch`. The API schema accepts the kind, but
 * upstream's `ChatMessageKind` union does not list it, so the fork's code
 * asks here instead of extending that union where upstream adds its own
 * kinds (M-141).
 */
export function isRouterBatch(message: ChatMessage): boolean {
  return (message.message_kind as string | undefined) === "router_batch";
}

/**
 * Drops the `no_response` rows of Router batch turns: the reply of a turn
 * whose input is a batch, matched by the task that owns that input. A batch
 * turn is never auto-retried (the server re-dispatches a failed batch as a new
 * turn instead), so its reply always carries the input owner's task id, and a
 * member message or failure row in between does not matter.
 */
export function hideSilentRouterTurns(messages: ChatMessage[]): ChatMessage[] {
  const batchTasks = new Set<string>();
  for (const m of messages) {
    if (isRouterBatch(m) && m.task_id) batchTasks.add(m.task_id);
  }
  if (batchTasks.size === 0) return messages;
  return messages.filter(
    (m) => m.message_kind !== "no_response" || !m.task_id || !batchTasks.has(m.task_id),
  );
}

/** The batch's first two lines: the server's label and its one-line summary. */
export function routerBatchHeadline(content: string): { label: string; summary: string } {
  const [label = "", summary = ""] = content.split("\n", 2);
  return { label: label.trim(), summary: summary.trim() };
}

export function RouterBatchMessage({ message }: { message: ChatMessage }) {
  const [open, setOpen] = useState(false);
  const { label, summary } = routerBatchHeadline(message.content);
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex w-full items-start gap-1.5 rounded-lg border border-dashed px-2.5 py-1.5 text-left text-caption text-muted-foreground hover:bg-accent/30 transition-colors">
        {open ? <ChevronDown className="mt-0.5 size-3 shrink-0" /> : <ChevronRight className="mt-0.5 size-3 shrink-0" />}
        <span className="min-w-0">
          <span className="font-medium">{label}</span>
          {summary && <span className="block truncate">{summary}</span>}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="mt-1 rounded-lg border bg-muted/20 px-3 py-2 text-body break-words">
          <RichContent content={message.content} density="compact" phase="settled" />
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
