"use client";

// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: confirm manual endpoint hosts before connecting (M-295 stage 2).

import { useState } from "react";
import { toast } from "sonner";
import { Loader2 } from "@multica/ui/icons";
import type { McpOAuthConnectionState } from "@multica/core/mcp-oauth";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@multica/ui/components/ui/tooltip";
import { useT } from "../i18n";
import { currentPath, useNavigation } from "../navigation";
import { isDesktopShell, openExternal } from "../platform";

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

/**
 * Connect and disconnect for the OAuth entries of one list. `pending` is the
 * key of the entry whose action is running. A connect sends the browser to
 * the authorization server and comes back to the current page; on desktop
 * the system browser opens and this page stays.
 */
export function useMcpOAuthActions() {
  const { t } = useT("mcpoauth");
  const navigation = useNavigation();
  const [pending, setPending] = useState<string | null>(null);

  const connect = (key: string, start: (returnTo: string) => Promise<string>) => {
    setPending(key);
    start(currentPath({ ...navigation, hash: "" })).then(
      (url) => {
        openExternal(url, { webTarget: "same-tab" });
        if (isDesktopShell()) {
          setPending(null);
          toast.message(t(($) => $.connect_continue));
        }
      },
      (error) => {
        setPending(null);
        toast.error(errorMessage(error, t(($) => $.action_failed)));
      },
    );
  };

  const disconnect = (key: string, name: string, run: () => Promise<unknown>) => {
    setPending(key);
    run().then(
      () => {
        setPending(null);
        toast.success(t(($) => $.disconnected_toast, { name }));
      },
      (error) => {
        setPending(null);
        toast.error(errorMessage(error, t(($) => $.action_failed)));
      },
    );
  };

  return { pending, connect, disconnect };
}

/** The hosts of an entry's manual OAuth endpoints. */
export interface McpOAuthEndpointHosts {
  authorization: string;
  token: string;
}

/** The hosts to confirm before connecting, or null for an entry without manual endpoints. */
export function mcpOAuthEndpointHosts(entry: { authorization_host: string; token_host: string }): McpOAuthEndpointHosts | null {
  return entry.authorization_host ? { authorization: entry.authorization_host, token: entry.token_host } : null;
}

/**
 * The connection state of one OAuth MCP entry and its actions, shown inline
 * in an MCP server row. `problem` is why the entry cannot be connected (an
 * invalid OAuth entry); then only that is shown. An entry with manual
 * endpoints (`endpointHosts`) asks first: its editor chose where the browser
 * goes and where the code is sent, so the person connecting sees the hosts.
 */
export function McpOAuthConnection({
  name,
  note,
  state,
  problem,
  canConnect,
  canDisconnect,
  pending,
  endpointHosts = null,
  onConnect,
  onDisconnect,
}: {
  name: string;
  /** Who the connection belongs to, when it is shared (a workspace entry). */
  note?: string;
  state: McpOAuthConnectionState;
  problem?: string;
  canConnect: boolean;
  canDisconnect: boolean;
  pending: boolean;
  endpointHosts?: McpOAuthEndpointHosts | null;
  onConnect: () => void;
  onDisconnect: () => void;
}) {
  const { t } = useT("mcpoauth");
  const [confirming, setConfirming] = useState(false);
  if (problem) {
    return (
      <Tooltip>
        <TooltipTrigger
          render={
            <span tabIndex={0} className="shrink-0 rounded-full outline-none focus-visible:ring-3 focus-visible:ring-ring/50">
              <Badge variant="destructive">{t(($) => $.status.invalid)}</Badge>
            </span>
          }
        />
        <TooltipContent className="max-w-72">{problem}</TooltipContent>
      </Tooltip>
    );
  }
  return (
    <div className="flex shrink-0 items-center gap-2">
      {note ? (
        <span className="hidden max-w-56 truncate text-caption text-muted-foreground md:inline" title={note}>
          {note}
        </span>
      ) : null}
      <Badge variant={state === "connected" ? "secondary" : state === "needs_reconnect" ? "destructive" : "outline"}>
        {t(($) => $.status[state])}
      </Badge>
      {canConnect && state !== "connected" && (
        <Button
          type="button"
          size="sm"
          disabled={pending}
          aria-busy={pending || undefined}
          aria-label={
            state === "needs_reconnect"
              ? t(($) => $.reconnect_aria, { name })
              : t(($) => $.connect_aria, { name })
          }
          onClick={() => (endpointHosts ? setConfirming(true) : onConnect())}
        >
          {pending && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
          {state === "needs_reconnect" ? t(($) => $.reconnect) : t(($) => $.connect)}
        </Button>
      )}
      {canDisconnect && state !== "not_connected" && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending}
          aria-label={t(($) => $.disconnect_aria, { name })}
          onClick={onDisconnect}
        >
          {t(($) => $.disconnect)}
        </Button>
      )}
      {endpointHosts ? (
        <AlertDialog open={confirming} onOpenChange={setConfirming}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.endpoints_confirm.title, { name })}</AlertDialogTitle>
              <AlertDialogDescription>{t(($) => $.endpoints_confirm.description)}</AlertDialogDescription>
            </AlertDialogHeader>
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-body">
              <dt className="text-muted-foreground">{t(($) => $.endpoints_confirm.authorization)}</dt>
              <dd className="break-all font-mono">{endpointHosts.authorization}</dd>
              <dt className="text-muted-foreground">{t(($) => $.endpoints_confirm.token)}</dt>
              <dd className="break-all font-mono">{endpointHosts.token}</dd>
            </dl>
            <AlertDialogFooter>
              <AlertDialogCancel>{t(($) => $.endpoints_confirm.cancel)}</AlertDialogCancel>
              <AlertDialogAction
                onClick={() => {
                  setConfirming(false);
                  onConnect();
                }}
              >
                {t(($) => $.endpoints_confirm.continue)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      ) : null}
    </div>
  );
}
