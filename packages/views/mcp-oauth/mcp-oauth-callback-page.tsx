"use client";

// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

import { useEffect, useRef, useState } from "react";
import { AlertCircle, Loader2 } from "@multica/ui/icons";
import { ApiError } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { completeMcpOAuth } from "@multica/core/mcp-oauth";
import { paths } from "@multica/core/paths";
import { Button } from "@multica/ui/components/ui/button";
import { useNavigation } from "../navigation";
import { useT } from "../i18n";

type Failure = "missing" | "denied" | "expired" | "not_allowed" | "sign_in" | "generic";

/**
 * Where an OAuth authorization server sends the browser after the agent
 * owner connects an MCP server (server: mcpoauth.CallbackPath).
 *
 * It posts the code and state to POST /api/mcp-oauth/complete with this
 * browser's own session. That is the point of the page: the server finishes a
 * connect only for the user who started it, so someone else's authorization
 * can never be attached to an agent by tricking them into opening a link.
 *
 * A signed-out browser is not sent to the login page with the code in tow:
 * the login flow can carry `next` into another provider's OAuth state, and an
 * authorization code has no business there. The user signs in and starts the
 * connect again instead.
 *
 * Only `return_to` from the server's answer is used to navigate, and only
 * when it is an app path; the query string never chooses where to go.
 */
export function McpOAuthCallbackPage() {
  const { t } = useT("mcpoauth");
  const navigation = useNavigation();
  const user = useAuthStore((s) => s.user);
  const isAuthLoading = useAuthStore((s) => s.isLoading);
  const [failure, setFailure] = useState<Failure | null>(null);
  const started = useRef(false);

  const state = navigation.searchParams.get("state") ?? "";
  const code = navigation.searchParams.get("code") ?? "";
  const error = navigation.searchParams.get("error") ?? "";

  useEffect(() => {
    if (started.current || isAuthLoading) return;
    if (!state) {
      setFailure("missing");
      return;
    }
    if (!user) {
      setFailure("sign_in");
      return;
    }
    started.current = true;
    completeMcpOAuth({ state, code, error }).then(
      (result) => {
        navigation.replace(isAppPath(result.return_to) ? result.return_to : paths.root());
      },
      (err: unknown) => setFailure(failureOf(err)),
    );
  }, [code, error, isAuthLoading, navigation, state, user]);

  if (failure) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-background p-6">
        <div className="flex max-w-[60ch] flex-col items-start gap-3" role="alert" aria-live="polite">
          <div className="flex items-center gap-2 text-body font-medium">
            <AlertCircle className="size-4 text-destructive" aria-hidden="true" />
            <span>{t(($) => $.callback.failed_title)}</span>
          </div>
          <p className="text-caption leading-5 text-muted-foreground">{t(($) => $.callback[failure])}</p>
          <Button className="h-11" onClick={() => navigation.replace(paths.root())}>
            {t(($) => $.callback.go_to_app)}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="flex min-h-dvh items-center justify-center bg-background p-6" role="status" aria-live="polite">
      <div className="flex items-center gap-3 text-body text-muted-foreground">
        <Loader2 className="size-4 animate-spin motion-reduce:animate-none" aria-hidden="true" />
        <span>{t(($) => $.callback.working)}</span>
      </div>
    </div>
  );
}

const PROBE_ORIGIN = "https://app.invalid";

/**
 * An in-app path. It is resolved the way the browser will resolve it (which
 * drops tab and newline characters, so "/\t/evil.example" means
 * "//evil.example") and accepted only if it stays on this origin. Control
 * characters are refused outright.
 */
export function isAppPath(path: string): boolean {
  // eslint-disable-next-line no-control-regex -- refusing control characters is the point
  if (!path.startsWith("/") || /[\u0000-\u001f\u007f]/.test(path)) return false;
  try {
    return new URL(path, PROBE_ORIGIN).origin === PROBE_ORIGIN;
  } catch {
    return false;
  }
}

function failureOf(err: unknown): Failure {
  if (err instanceof ApiError) {
    const code = (err.body as { code?: unknown } | undefined)?.code;
    if (code === "mcp_oauth_denied") return "denied";
    if (code === "mcp_oauth_state_invalid") return "expired";
    if (code === "mcp_oauth_not_allowed") return "not_allowed";
    if (err.status === 401) return "sign_in";
  }
  return "generic";
}
