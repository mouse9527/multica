// Added by the mouse9527/multica fork: Router page settings (M-105).
//
// Settings → Routers: every Router of the member, one row per chat. A row
// opens its chat, where the Router is configured. Below them, the member's
// subscriptions not bound to a chat (kept when a Router was released), which
// can be deleted here or picked up again by setting a chat up with the same
// scope.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { chatSessionsOptions } from "@multica/core/chat/queries";
import { agentListOptions } from "@multica/core/workspace/queries";
import {
  groupRouters,
  routerSubscriptionsOptions,
  unboundSubscriptions,
  useLastRouterDispatch,
  useRemoveRouterScope,
  type RouterInstance,
  type RouterSubscription,
} from "@multica/core/router";
import type { Agent, ChatSession } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { AppLink } from "../navigation";
import { useT, useTimeAgo } from "../i18n";
import { SettingsTab } from "../settings/components/settings-layout";
import { RouterStatusBadge, ScopeLabel, ScopePausedBadge } from "./router-labels";
import { RouterHeldItems } from "./router-chat-dialog";

export function RoutersTab() {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const userId = useAuthStore((s) => s.user?.id) ?? "";
  const subs = useQuery(routerSubscriptionsOptions(wsId));
  const sessions = useQuery(chatSessionsOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const loading = subs.isLoading || sessions.isLoading;
  const byId = new Map((sessions.data ?? []).map((s) => [s.id, s]));
  const routers = groupRouters(subs.data ?? [], new Set(byId.keys()));
  const unbound = unboundSubscriptions(subs.data ?? [], userId, new Map(agents.map((a) => [a.id, a.owner_id])));

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
            />
          ))}
        </ul>
      )}
      {unbound.length > 0 && <UnboundSubscriptions subs={unbound} agents={agents} />}
      <RouterHeldItems />
    </SettingsTab>
  );
}

function RouterRow({
  router,
  session,
  agent,
}: {
  router: RouterInstance;
  session: ChatSession | undefined;
  agent: Agent | undefined;
}) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const timeAgo = useTimeAgo();
  const lastRun = useLastRouterDispatch(wsId, router.subscriptions);
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
          <dd>{lastRun ? timeAgo(lastRun.created_at) : t(($) => $.health.no_runs)}</dd>
        </dl>
      </AppLink>
    </li>
  );
}

function UnboundSubscriptions({ subs, agents }: { subs: RouterSubscription[]; agents: Agent[] }) {
  const { t } = useT("router");
  const remove = useRemoveRouterScope();
  const [deleting, setDeleting] = useState<RouterSubscription | null>(null);

  const confirmDelete = () => {
    const sub = deleting;
    setDeleting(null);
    if (!sub) return;
    remove.mutate(sub, {
      onSuccess: () => toast.success(t(($) => $.toast.deleted)),
      onError: (e) => toast.error(t(($) => $.toast.failed, { error: e instanceof Error ? e.message : String(e) })),
    });
  };

  return (
    <section className="space-y-2" data-testid="router-unbound">
      <h3 className="text-body font-medium">{t(($) => $.list.unbound_title)}</h3>
      <p className="text-caption text-muted-foreground">{t(($) => $.list.unbound_hint)}</p>
      <ul className="space-y-1">
        {subs.map((sub) => (
          <li key={sub.id} className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border px-3 py-2 text-body">
            <span className="min-w-0 flex-1">
              <ScopeLabel scopeType={sub.scope_type} scopeId={sub.scope_id} />
            </span>
            <span className="text-caption text-muted-foreground">{agents.find((a) => a.id === sub.agent_id)?.name ?? "—"}</span>
            <ScopePausedBadge sub={sub} />
            <Button
              variant="ghost"
              size="sm"
              className="text-destructive"
              disabled={remove.isPending}
              onClick={() => setDeleting(sub)}
            >
              {t(($) => $.list.delete)}
            </Button>
          </li>
        ))}
      </ul>
      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.list.delete_title)}</AlertDialogTitle>
            <AlertDialogDescription>{t(($) => $.list.delete_description)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.actions.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={confirmDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t(($) => $.list.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
