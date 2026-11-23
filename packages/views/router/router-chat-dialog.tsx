// Added by the mouse9527/multica fork: Router page settings (M-105), and the
// overlap refusal on subscribe (M-194).
//
// "Set as Router" / "Router settings" for one chat. A Router is the chat plus
// the subscriptions bound to it; its agent is the chat's agent. Every write
// goes through the Router API, which checks that the chat is the member's
// own, active, and with the subscribed agent.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  reconciledThrough,
  routerChangesOptions,
  routerStatus,
  routerSubscriptionsOfChat,
  routerSubscriptionsOptions,
  RouterScopeOverlapError,
  RouterScopeTakenError,
  useLastRouterDispatch,
  useAddRouterScopes,
  useReleaseRouter,
  useRemoveRouterScope,
  useSetRouterPaused,
  useUpdateRouterSettings,
  type AddRouterScopesResult,
  type RouterOverlap,
  type RouterReleaseMode,
  type RouterSettingsInput,
  type RouterSubscription,
} from "@multica/core/router";
import type { ChatSession } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
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
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { X } from "@multica/ui/icons";
import { useT, useTimeAgo } from "../i18n";
import { DEFAULT_ROUTER_SETTINGS, settingsChanged, settingsOf, validSettings, type PendingScope } from "./router-form";
import { BoardLink, RouterStatusBadge, ScopeLabel, ScopePausedBadge } from "./router-labels";
import { overlapsOf, RouterOverlapWarning } from "./router-overlap-warning";
import { RouterScopePicker } from "./router-scope-picker";
import { RouterSettingsFields } from "./router-settings-fields";

function errorText(e: unknown): string {
  return e instanceof Error && e.message ? e.message : String(e);
}

export function RouterChatDialog({
  session,
  agentName,
  open,
  onOpenChange,
}: {
  session: ChatSession;
  agentName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const { data: subs, isLoading } = useQuery({ ...routerSubscriptionsOptions(wsId), enabled: open });
  const bound = routerSubscriptionsOfChat(subs ?? [], session.id);
  const isRouter = bound.length > 0;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[90%] flex-col gap-0 p-0 sm:max-w-xl">
        <DialogHeader className="border-b px-5 py-4">
          <DialogTitle>{isRouter ? t(($) => $.dialog.title_settings) : t(($) => $.dialog.title_setup)}</DialogTitle>
          <DialogDescription>{t(($) => $.dialog.description, { agent: agentName })}</DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <div className="space-y-3 p-5">
            <Skeleton className="h-6 w-1/2" />
            <Skeleton className="h-24 w-full" />
          </div>
        ) : isRouter ? (
          // Keyed by the subscription set so the form restarts from the
          // server's values when scopes come or go.
          <RouterSettingsBody
            key={bound.map((s) => s.id).join()}
            session={session}
            subs={bound}
            allSubs={subs ?? []}
            onDone={() => onOpenChange(false)}
          />
        ) : session.status === "archived" ? (
          <p className="p-5 text-body text-muted-foreground">{t(($) => $.dialog.archived)}</p>
        ) : (
          <RouterSetupBody session={session} allSubs={subs ?? []} onDone={() => onOpenChange(false)} />
        )}
      </DialogContent>
    </Dialog>
  );
}

function useBoard(session: ChatSession) {
  const { t } = useT("router");
  const { t: tChat } = useT("chat");
  const chat = session.title?.trim() || tChat(($) => $.window.untitled);
  return { title: t(($) => $.board.title, { chat }), description: t(($) => $.board.description, { chat }) };
}

/**
 * Toasts each scope that failed, except an overlap, which the form shows
 * (RouterOverlapWarning); returns whether any was created.
 */
function useReportAdded() {
  const { t } = useT("router");
  return (result: AddRouterScopesResult, labels: Map<string, string>) => {
    for (const f of result.failed) {
      if (f.error instanceof RouterScopeOverlapError) continue;
      const scope = labels.get(f.scope.scope_id) ?? f.scope.scope_id;
      toast.error(
        f.error instanceof RouterScopeTakenError
          ? t(($) => $.toast.scope_taken, { scope })
          : t(($) => $.toast.scope_failed, { scope, error: errorText(f.error) }),
      );
    }
    return result.created.length > 0;
  };
}

/**
 * Scopes this chat cannot add: its own, and those the agent already covers
 * in another chat's Router (an agent subscribes to a scope once). Unbound
 * subscriptions of the agent stay pickable; adding one takes it over.
 */
function takenScopes(allSubs: RouterSubscription[], session: ChatSession): string[] {
  return allSubs
    .filter((s) => s.agent_id === session.agent_id && s.anchor_chat_session_id)
    .map((s) => s.scope_id);
}

function RouterSetupBody({
  session,
  allSubs,
  onDone,
}: {
  session: ChatSession;
  allSubs: RouterSubscription[];
  onDone: () => void;
}) {
  const { t } = useT("router");
  const board = useBoard(session);
  const report = useReportAdded();
  const add = useAddRouterScopes();
  const [scopes, setScopes] = useState<PendingScope[]>([]);
  const [settings, setSettings] = useState<RouterSettingsInput>(DEFAULT_ROUTER_SETTINGS);
  const [overlaps, setOverlaps] = useState<Map<string, RouterOverlap[]>>(() => new Map());
  const busy = add.isPending;

  const submit = async () => {
    const result = await add.mutateAsync({
      chatSessionId: session.id,
      agentId: session.agent_id,
      scopes: scopes.map(({ scope_type, scope_id }) => ({ scope_type, scope_id })),
      settings,
      board,
      existing: allSubs,
      // Shown the overlap, the member submitted again: add those anyway.
      allowOverlap: [...overlaps.keys()],
    });
    const created = new Set(result.created.map((s) => s.scope_id));
    // Keep what failed in the form so it can be retried.
    setScopes((prev) => prev.filter((s) => !created.has(s.scope_id)));
    setOverlaps(overlapsOf(result));
    if (report(result, new Map(scopes.map((s) => [s.scope_id, s.label])))) {
      toast.success(t(($) => $.toast.created));
      if (result.failed.length === 0) onDone();
    }
  };

  return (
    <>
      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-5 py-4">
        <section className="space-y-2">
          <h3 className="text-body font-medium">{t(($) => $.scopes.title)}</h3>
          <RouterScopePicker pending={scopes} onChange={setScopes} taken={takenScopes(allSubs, session)} disabled={busy} />
          {scopes.some((s) => s.scope_type === "project") && (
            <p className="text-caption text-muted-foreground">{t(($) => $.scopes.board_hint)}</p>
          )}
          <RouterOverlapWarning
            overlaps={overlaps}
            labels={new Map(scopes.map((s) => [s.scope_id, s.label]))}
            action={t(($) => $.actions.create)}
          />
        </section>
        <RouterSettingsFields value={settings} onChange={setSettings} disabled={busy} />
      </div>
      <DialogFooter className="mx-0 mb-0 px-5 py-3">
        <Button variant="outline" onClick={onDone} disabled={busy}>
          {t(($) => $.actions.cancel)}
        </Button>
        <Button onClick={submit} disabled={busy || scopes.length === 0 || !validSettings(settings)} aria-busy={busy}>
          {t(($) => $.actions.create)}
        </Button>
      </DialogFooter>
    </>
  );
}

function RouterSettingsBody({
  session,
  subs,
  allSubs,
  onDone,
}: {
  session: ChatSession;
  subs: RouterSubscription[];
  allSubs: RouterSubscription[];
  onDone: () => void;
}) {
  const { t } = useT("router");
  const board = useBoard(session);
  const report = useReportAdded();
  const add = useAddRouterScopes();
  const update = useUpdateRouterSettings();
  const pause = useSetRouterPaused();
  const remove = useRemoveRouterScope();
  const release = useReleaseRouter();
  const [pending, setPending] = useState<PendingScope[]>([]);
  const [settings, setSettings] = useState<RouterSettingsInput>(() => settingsOf(subs));
  const [overlaps, setOverlaps] = useState<Map<string, RouterOverlap[]>>(() => new Map());
  const [releasing, setReleasing] = useState(false);
  const busy = add.isPending || update.isPending || pause.isPending || remove.isPending || release.isPending;
  const paused = routerStatus(subs).kind === "paused";
  const dirty = pending.length > 0 || settingsChanged(subs, settings);

  const fail = (e: unknown) => toast.error(t(($) => $.toast.failed, { error: errorText(e) }));

  const save = async () => {
    try {
      if (settingsChanged(subs, settings)) await update.mutateAsync({ subs, settings });
      if (pending.length > 0) {
        const result = await add.mutateAsync({
          chatSessionId: session.id,
          agentId: session.agent_id,
          scopes: pending.map(({ scope_type, scope_id }) => ({ scope_type, scope_id })),
          settings,
          board,
          existing: allSubs,
          allowOverlap: [...overlaps.keys()],
        });
        const created = new Set(result.created.map((s) => s.scope_id));
        setPending((prev) => prev.filter((s) => !created.has(s.scope_id)));
        setOverlaps(overlapsOf(result));
        report(result, new Map(pending.map((s) => [s.scope_id, s.label])));
        if (result.failed.length > 0) return;
      }
      toast.success(t(($) => $.toast.saved));
    } catch (e) {
      fail(e);
    }
  };

  const togglePause = async () => {
    try {
      await pause.mutateAsync({ subs, paused: !paused });
      toast.success(paused ? t(($) => $.toast.resumed) : t(($) => $.toast.paused));
    } catch (e) {
      fail(e);
    }
  };

  const doRelease = async (mode: RouterReleaseMode) => {
    setReleasing(false);
    try {
      await release.mutateAsync({ subs, mode });
      toast.success(t(($) => $.toast.released));
      onDone();
    } catch (e) {
      fail(e);
    }
  };

  return (
    <>
      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-5 py-4">
        <RouterStatusBadge subs={subs} />
        <section className="space-y-2">
          <h3 className="text-body font-medium">{t(($) => $.scopes.title)}</h3>
          <ul className="space-y-1">
            {subs.map((sub) => (
              <li key={sub.id} className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border px-2 py-1.5 text-body">
                <span className="min-w-0 flex-1">
                  <ScopeLabel scopeType={sub.scope_type} scopeId={sub.scope_id} />
                </span>
                <ScopePausedBadge sub={sub} />
                {sub.scope_type === "project" && sub.anchor_issue_id && <BoardLink issueId={sub.anchor_issue_id} />}
                {/* The last scope goes with the whole Router (Release). */}
                {subs.length > 1 && (
                  <Button
                    variant="ghost"
                    size="icon-xs"
                    aria-label={t(($) => $.scopes.remove)}
                    disabled={busy}
                    onClick={() => remove.mutate(sub, { onError: fail })}
                  >
                    <X />
                  </Button>
                )}
              </li>
            ))}
          </ul>
          <RouterScopePicker pending={pending} onChange={setPending} taken={takenScopes(allSubs, session)} disabled={busy} />
          {pending.some((s) => s.scope_type === "project") && (
            <p className="text-caption text-muted-foreground">{t(($) => $.scopes.board_hint)}</p>
          )}
          <RouterOverlapWarning
            overlaps={overlaps}
            labels={new Map(pending.map((s) => [s.scope_id, s.label]))}
            action={t(($) => $.actions.save)}
          />
        </section>
        <RouterSettingsFields value={settings} onChange={setSettings} disabled={busy} />
        <RouterHealth subs={subs} />
        <RouterHeldItems />
      </div>
      <DialogFooter className="mx-0 mb-0 flex-row flex-wrap items-center justify-between px-5 py-3">
        <Button variant="ghost" className="text-destructive" onClick={() => setReleasing(true)} disabled={busy}>
          {t(($) => $.actions.release)}
        </Button>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" onClick={togglePause} disabled={busy}>
            {paused ? t(($) => $.actions.resume) : t(($) => $.actions.pause)}
          </Button>
          <Button onClick={save} disabled={busy || !dirty || !validSettings(settings)} aria-busy={busy}>
            {t(($) => $.actions.save)}
          </Button>
        </div>
      </DialogFooter>
      <ReleaseRouterDialog open={releasing} onOpenChange={setReleasing} onConfirm={doRelease} />
    </>
  );
}

function ReleaseRouterDialog({
  open,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: (mode: RouterReleaseMode) => void;
}) {
  const { t } = useT("router");
  const [mode, setMode] = useState<RouterReleaseMode>("delete");
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t(($) => $.release.title)}</AlertDialogTitle>
          <AlertDialogDescription>{t(($) => $.release.description)}</AlertDialogDescription>
        </AlertDialogHeader>
        <div role="radiogroup" aria-label={t(($) => $.release.title)} className="space-y-2 text-body">
          {(["delete", "keep_paused"] as const).map((value) => (
            <label key={value} className="flex items-center gap-2">
              <input
                type="radio"
                name="router-release-mode"
                value={value}
                checked={mode === value}
                onChange={() => setMode(value)}
              />
              {value === "delete" ? t(($) => $.release.delete) : t(($) => $.release.keep)}
            </label>
          ))}
        </div>
        {mode === "keep_paused" && <p className="text-caption text-muted-foreground">{t(($) => $.release.keep_hint)}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel>{t(($) => $.actions.cancel)}</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => onConfirm(mode)}
            className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
          >
            {t(($) => $.release.confirm)}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/** Capture coverage and the last run, so the member can tell it is alive. */
export function RouterHealth({ subs }: { subs: RouterSubscription[] }) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const timeAgo = useTimeAgo();
  const last = useLastRouterDispatch(wsId, subs);
  const through = reconciledThrough(subs);
  const misses = subs.reduce((n, s) => n + s.capture_miss, 0);
  const outcome = !last
    ? null
    : last.outcome === "completed"
      ? t(($) => $.health.outcome.completed)
      : last.outcome === "failed"
        ? t(($) => $.health.outcome.failed)
        : t(($) => $.health.outcome.running);
  const autoPaused = subs.find((s) => s.enabled && s.paused_reason === "agent_unavailable");

  return (
    <section className="space-y-2">
      <h3 className="text-body font-medium">{t(($) => $.health.title)}</h3>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-body">
        <dt className="text-muted-foreground">{t(($) => $.health.last_run)}</dt>
        <dd>
          {last
            ? `${timeAgo(last.created_at)} · ${outcome} · ${t(($) => $.health.run_events, { count: last.event_count })}`
            : t(($) => $.health.no_runs)}
        </dd>
        <dt className="text-muted-foreground">{t(($) => $.health.reconciled)}</dt>
        <dd>{through ? timeAgo(through) : t(($) => $.health.never)}</dd>
        <dt className="text-muted-foreground">{t(($) => $.health.capture_miss)}</dt>
        <dd>{misses}</dd>
      </dl>
      {autoPaused && <PauseWhy subscriptionId={autoPaused.id} />}
    </section>
  );
}

/**
 * The server records why dispatch paused itself (agent archived, chat
 * archived, project access refused) on the subscription's last auto_pause.
 */
function PauseWhy({ subscriptionId }: { subscriptionId: string }) {
  const wsId = useWorkspaceId();
  const { data: changes = [] } = useQuery(routerChangesOptions(wsId, subscriptionId));
  const last = [...changes].reverse().find((c) => c.action === "auto_pause");
  const why = typeof last?.detail.why === "string" ? last.detail.why : null;
  if (!why) return null;
  return <p className="rounded-md bg-muted/40 px-2 py-1.5 text-caption text-muted-foreground break-words">{why}</p>;
}

/**
 * Reserved for slices 2 and 3 of Router v6: inbox items the Router screened
 * before they reached the member will be listed here.
 */
export function RouterHeldItems() {
  const { t } = useT("router");
  return (
    <section className="space-y-1" data-testid="router-held-items">
      <h3 className="text-body font-medium">{t(($) => $.held.title)}</h3>
      <p className="text-caption text-muted-foreground">{t(($) => $.held.empty)}</p>
    </section>
  );
}
