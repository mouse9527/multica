// Added by the mouse9527/multica fork: which Super Agents cover an issue (M-194).

import { useQuery } from "@tanstack/react-query";
import { useFeatureEnabled } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import { ROUTER_CHAT_FLAG, routerIssueCoveringOptions, type RouterCovering } from "@multica/core/router";
import { Badge } from "@multica/ui/components/ui/badge";
import { FolderKanban, ListTree } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { PropRow } from "../common/prop-row";
import { useT } from "../i18n";
import { usePauseReasonLabel } from "./router-labels";

/**
 * The issue sidebar's "Super Agent" row: every subscription whose scope
 * contains the issue. Each agent receives the issue's events from its
 * narrowest one; a wider one is shown dimmed. Nothing while `router_chat` is
 * off.
 */
export function IssueSuperAgentsRow({ issueId }: { issueId: string }) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const enabled = useFeatureEnabled(ROUTER_CHAT_FLAG, false);
  const { data: covering } = useQuery({
    ...routerIssueCoveringOptions(wsId, issueId),
    enabled: enabled && Boolean(issueId),
    retry: false,
  });
  if (!enabled || !covering) return null;
  return (
    <PropRow interactive={false} label={t(($) => $.covering.label)} className={covering.length > 0 ? "items-start py-1.5" : undefined}>
      {covering.length === 0 ? (
        <span className="text-muted-foreground">{t(($) => $.covering.none)}</span>
      ) : (
        <ul className="flex min-w-0 flex-1 flex-col gap-1">
          {covering.map((c) => (
            <CoveringItem key={c.subscription_id} covering={c} />
          ))}
        </ul>
      )}
    </PropRow>
  );
}

function CoveringItem({ covering: c }: { covering: RouterCovering }) {
  const { t } = useT("router");
  const reasonLabel = usePauseReasonLabel();
  const project = c.scope_type === "project";
  const Icon = project ? FolderKanban : ListTree;
  const kind = project ? t(($) => $.scopes.project) : t(($) => $.scopes.issue_tree);
  const paused = !c.enabled || c.paused_reason !== null;
  const chat = c.chat_title?.trim() || (c.chat_session_id ? t(($) => $.list.untitled) : null);
  return (
    <li className={cn("flex min-w-0 flex-col", !c.effective && "text-muted-foreground")}>
      <span className="flex min-w-0 items-center gap-1.5">
        <span className="truncate font-medium">{c.agent_name || t(($) => $.scopes.unknown)}</span>
        {paused && (
          <Badge variant="outline" className="shrink-0">
            {reasonLabel(c.paused_reason ?? "manual")}
          </Badge>
        )}
      </span>
      <span className="flex min-w-0 items-center gap-1 text-caption text-muted-foreground">
        <Icon className="size-3 shrink-0" aria-label={kind} />
        <span className="truncate">{c.scope_name || t(($) => $.scopes.unknown)}</span>
        {chat && <span className="truncate">· {chat}</span>}
      </span>
      {c.enabled && !c.effective && (
        <span className="text-caption text-muted-foreground">{t(($) => $.covering.superseded)}</span>
      )}
    </li>
  );
}
