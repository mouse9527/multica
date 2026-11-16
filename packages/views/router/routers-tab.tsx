// Added by the mouse9527/multica fork: Router page settings (M-105).
//
// Settings → Routers: every Router of the member, one row per chat. A row
// opens its chat, where the Router is configured.

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { chatSessionsOptions } from "@multica/core/chat/queries";
import { agentListOptions } from "@multica/core/workspace/queries";
import {
  groupRouters,
  latestDispatch,
  routerDispatchesOptions,
  routerSubscriptionsOptions,
  type RouterInstance,
} from "@multica/core/router";
import type { Agent, ChatSession } from "@multica/core/types";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { AppLink } from "../navigation";
import { useT, useTimeAgo } from "../i18n";
import { SettingsTab } from "../settings/components/settings-layout";
import { RouterStatusBadge, ScopeLabel } from "./router-labels";
import { RouterHeldItems } from "./router-chat-dialog";

export function RoutersTab() {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const subs = useQuery(routerSubscriptionsOptions(wsId));
  const sessions = useQuery(chatSessionsOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: dispatches = [] } = useQuery(routerDispatchesOptions(wsId));
  const loading = subs.isLoading || sessions.isLoading;
  const byId = new Map((sessions.data ?? []).map((s) => [s.id, s]));
  const routers = groupRouters(subs.data ?? [], new Set(byId.keys()));

  return (
    <SettingsTab title={t(($) => $.list.tab)}>
      {loading ? (
        <div className="space-y-2">
          <Skeleton className="h-16 w-full" />
          <Skeleton className="h-16 w-full" />
        </div>
      ) : routers.length === 0 ? (
        <p className="text-body text-muted-foreground">{t(($) => $.list.empty)}</p>
      ) : (
        <ul className="space-y-2">
          {routers.map((router) => (
            <RouterRow
              key={router.chat_session_id}
              router={router}
              session={byId.get(router.chat_session_id)}
              agent={agents.find((a) => a.id === router.agent_id)}
              lastRunAt={latestDispatch(dispatches, router.subscriptions)?.created_at ?? null}
            />
          ))}
        </ul>
      )}
      <RouterHeldItems />
    </SettingsTab>
  );
}

function RouterRow({
  router,
  session,
  agent,
  lastRunAt,
}: {
  router: RouterInstance;
  session: ChatSession | undefined;
  agent: Agent | undefined;
  lastRunAt: string | null;
}) {
  const { t } = useT("router");
  const paths = useWorkspacePaths();
  const timeAgo = useTimeAgo();
  return (
    <li>
      <AppLink
        href={paths.chatSession(router.chat_session_id)}
        className="block space-y-2 rounded-lg border px-4 py-3 transition-colors hover:bg-accent/40"
      >
        <div className="flex flex-wrap items-center justify-between gap-2">
          <span className="min-w-0 truncate text-body font-medium">{session?.title?.trim() || t(($) => $.list.untitled)}</span>
          <RouterStatusBadge subs={router.subscriptions} />
        </div>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-caption">
          <dt className="text-muted-foreground">{t(($) => $.list.agent)}</dt>
          <dd className="truncate">{agent?.name ?? "—"}</dd>
          <dt className="text-muted-foreground">{t(($) => $.list.scopes)}</dt>
          <dd className="flex min-w-0 flex-col gap-0.5">
            {router.subscriptions.map((s) => (
              <ScopeLabel key={s.id} scopeType={s.scope_type} scopeId={s.scope_id} />
            ))}
          </dd>
          <dt className="text-muted-foreground">{t(($) => $.list.last_run)}</dt>
          <dd>{lastRunAt ? timeAgo(lastRunAt) : t(($) => $.health.no_runs)}</dd>
        </dl>
      </AppLink>
    </li>
  );
}
