// Added by the mouse9527/multica fork: Router marker in the chat list (M-108).
//
// A radar beside a chat's title says the chat is a Router: it watches its
// scopes for the member. Brand-coloured while it runs; grey with a pause mark
// while any scope is paused. The tooltip names the scopes and the status.
// While `router_chat` is off it renders nothing and touches no query, so the
// chat list and header behave as upstream.

import { useQuery } from "@tanstack/react-query";
import { useFeatureEnabled } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import { ROUTER_CHAT_FLAG, routerChatsOptions, type RouterChatSummary } from "@multica/core/router";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { CirclePause, Radar } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { DeferredTooltip } from "../common/deferred-tooltip";
import { useT } from "../i18n";
import { usePauseReasonLabel } from "./router-labels";

/** The Router summary of one chat, or null when it is not a Router (or the feature is off). */
export function useRouterChatSummary(sessionId: string): RouterChatSummary | null {
  const enabled = useFeatureEnabled(ROUTER_CHAT_FLAG, false);
  const wsId = useWorkspaceId();
  const { data } = useQuery({ ...routerChatsOptions(wsId), enabled });
  if (!enabled) return null;
  return data?.find((c) => c.chat_session_id === sessionId) ?? null;
}

/** "Router · Scopes: multica project · Running" for the tooltip and the accessible name. */
export function useRouterMarkerLabel() {
  const { t } = useT("router");
  const reasonLabel = usePauseReasonLabel();
  return (summary: RouterChatSummary) => {
    const paused = summary.paused_reasons.length > 0;
    // Mark single paused scopes only when the Router is partly paused.
    const partly = paused && summary.scopes.some((s) => !s.paused_reason);
    const scopes = summary.scopes
      .map((s) => {
        const name = !s.visible
          ? t(($) => $.scopes.unknown)
          : s.scope_type === "project"
            ? t(($) => $.marker.scope_project, { name: s.name })
            : t(($) => $.marker.scope_issue_tree, { identifier: s.identifier, name: s.name });
        return partly && s.paused_reason ? t(($) => $.marker.scope_paused, { scope: name }) : name;
      })
      .join(t(($) => $.marker.separator));
    const status = paused
      ? t(($) => $.marker.paused, {
          reasons: summary.paused_reasons.map(reasonLabel).join(t(($) => $.marker.separator)),
        })
      : t(($) => $.status.running);
    return t(($) => $.marker.label, { scopes, status });
  };
}

function MarkerIcon({ paused, className }: { paused: boolean; className?: string }) {
  return (
    <span className={cn("relative inline-flex shrink-0", paused && "mr-0.5", className)}>
      <Radar className={cn("size-full", paused ? "text-muted-foreground" : "text-brand")} />
      {paused && (
        <CirclePause
          strokeWidth={2.5}
          className="absolute -right-0.5 -bottom-0.5 size-[55%] rounded-full bg-background text-muted-foreground"
        />
      )}
    </span>
  );
}

/** The marker in a chat list row; nothing for a chat that is not a Router. */
export function RouterChatListMarker({ sessionId }: { sessionId: string }) {
  const summary = useRouterChatSummary(sessionId);
  const label = useRouterMarkerLabel();
  if (!summary) return null;
  const text = label(summary);
  const paused = summary.paused_reasons.length > 0;
  return (
    <DeferredTooltip
      trigger={
        <span role="img" aria-label={text} data-router-status={paused ? "paused" : "running"} className="inline-flex shrink-0">
          <MarkerIcon paused={paused} className="size-3.5" />
        </span>
      }
      content={text}
    />
  );
}

/** The marker in the chat header: a button that opens the Router settings. */
export function RouterChatHeaderMarker({ summary, onOpen }: { summary: RouterChatSummary; onOpen: () => void }) {
  const label = useRouterMarkerLabel();
  const text = label(summary);
  const paused = summary.paused_reasons.length > 0;
  return (
    <TooltipHint label={text} side="bottom">
      <button
        type="button"
        aria-label={text}
        data-router-status={paused ? "paused" : "running"}
        onClick={onOpen}
        className="inline-flex size-6 shrink-0 items-center justify-center rounded-sm outline-none hover:bg-accent focus-visible:ring-1 focus-visible:ring-ring"
      >
        <MarkerIcon paused={paused} className="size-4" />
      </button>
    </TooltipHint>
  );
}
