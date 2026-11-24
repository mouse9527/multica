"use client";

// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Globe2, Loader2, Plus, Server, Trash2 } from "@multica/ui/icons";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  mcpOAuthServersOptions,
  useConnectMcpOAuthServer,
  useCreateMcpOAuthServer,
  useDeleteMcpOAuthServer,
  useDisconnectMcpOAuthServer,
  type McpOAuthServer,
} from "@multica/core/mcp-oauth";
import type { Agent } from "@multica/core/types";
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
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@multica/ui/components/ui/field";
import { Input } from "@multica/ui/components/ui/input";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { useT } from "../../../i18n";
import { currentPath, useNavigation } from "../../../navigation";
import { isDesktopShell, openExternal } from "../../../platform";

type ConnectionState = "connected" | "needs_reconnect" | "not_connected";

function connectionState(server: McpOAuthServer): ConnectionState {
  if (!server.connection) return "not_connected";
  return server.connection.status === "connected" ? "connected" : "needs_reconnect";
}

/**
 * Parses the extra-headers box: one `Name: value` per line, blank lines
 * ignored. Returns null when a line is not in that form.
 */
export function parseExtraHeaders(text: string): Record<string, string> | null {
  const headers: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (line === "") continue;
    const colon = line.indexOf(":");
    if (colon <= 0) return null;
    const name = line.slice(0, colon).trim();
    const value = line.slice(colon + 1).trim();
    if (name === "" || value === "") return null;
    headers[name] = value;
  }
  return headers;
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

/**
 * An agent's OAuth MCP servers and the owner's connection to each. Runs use
 * the owner's connection; only the owner can connect, while the owner or a
 * workspace admin can add and remove servers (the server enforces both).
 */
export function McpOAuthTab({ agent }: { agent: Agent }) {
  const { t } = useT("mcpoauth");
  const wsId = useWorkspaceId();
  const navigation = useNavigation();
  const query = useQuery(mcpOAuthServersOptions(wsId, agent.id));
  const create = useCreateMcpOAuthServer(wsId, agent.id);
  const remove = useDeleteMcpOAuthServer(wsId, agent.id);
  const disconnect = useDisconnectMcpOAuthServer(wsId, agent.id);
  const connect = useConnectMcpOAuthServer(wsId, agent.id);
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<McpOAuthServer | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const servers = query.data?.servers ?? [];
  const canConnect = query.data?.can_connect ?? false;

  const failed = (error: unknown) => toast.error(errorMessage(error, t(($) => $.action_failed)));

  const handleConnect = (server: McpOAuthServer) => {
    setBusy(server.id);
    const returnTo = currentPath({ ...navigation, hash: "" });
    connect.mutateAsync({ serverId: server.id, returnTo }).then(
      (url) => {
        openExternal(url, { webTarget: "same-tab" });
        // Desktop opens the system browser and stays here.
        if (isDesktopShell()) {
          setBusy(null);
          toast.message(t(($) => $.connect_continue));
        }
      },
      (error) => {
        setBusy(null);
        failed(error);
      },
    );
  };

  const handleDisconnect = (server: McpOAuthServer) => {
    setBusy(server.id);
    disconnect.mutateAsync(server.id).then(
      () => {
        setBusy(null);
        toast.success(t(($) => $.disconnected_toast, { name: server.name }));
      },
      (error) => {
        setBusy(null);
        failed(error);
      },
    );
  };

  const handleDelete = () => {
    if (!deleting) return;
    const server = deleting;
    remove.mutateAsync(server.id).then(
      () => {
        setDeleting(null);
        toast.success(t(($) => $.deleted_toast, { name: server.name }));
      },
      (error) => {
        setDeleting(null);
        failed(error);
      },
    );
  };

  return (
    <div className="space-y-4">
      <section className="space-y-3">
        <div className="flex items-center justify-between gap-4">
          <h3 className="text-body font-medium">{t(($) => $.title)}</h3>
          <Button type="button" variant="outline" size="sm" onClick={() => setAdding(true)}>
            <Plus aria-hidden="true" />
            {t(($) => $.add)}
          </Button>
        </div>
        <p className="text-caption text-muted-foreground">{t(($) => $.hint)}</p>
        {query.isLoading ? (
          <Notice loading text={t(($) => $.loading)} />
        ) : query.isError ? (
          <Notice text={t(($) => $.load_failed)} />
        ) : servers.length === 0 ? (
          <Notice text={t(($) => $.empty)} />
        ) : (
          <ul className="divide-y rounded-lg border bg-surface-raised/40">
            {servers.map((server) => {
              const state = connectionState(server);
              const pending = busy === server.id;
              return (
                <li key={server.id} className="flex min-h-16 items-center gap-3 px-4 py-3">
                  <Globe2 className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-body font-medium">{server.name}</p>
                    <p className="truncate text-caption text-muted-foreground">{server.url}</p>
                  </div>
                  <Badge variant={state === "connected" ? "secondary" : state === "needs_reconnect" ? "destructive" : "outline"}>
                    {t(($) => $.status[state])}
                  </Badge>
                  {canConnect && state !== "connected" && (
                    <Button
                      type="button"
                      size="sm"
                      disabled={pending}
                      aria-busy={pending || undefined}
                      onClick={() => handleConnect(server)}
                    >
                      {pending && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
                      {state === "needs_reconnect" ? t(($) => $.reconnect) : t(($) => $.connect)}
                    </Button>
                  )}
                  {state !== "not_connected" && (
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      disabled={pending}
                      onClick={() => handleDisconnect(server)}
                    >
                      {t(($) => $.disconnect)}
                    </Button>
                  )}
                  <Button
                    type="button"
                    size="icon-sm"
                    variant="ghost"
                    aria-label={t(($) => $.delete_aria, { name: server.name })}
                    disabled={pending}
                    onClick={() => setDeleting(server)}
                  >
                    <Trash2 aria-hidden="true" />
                  </Button>
                </li>
              );
            })}
          </ul>
        )}
        {!canConnect && servers.some((server) => connectionState(server) !== "connected") && (
          <p className="text-caption text-muted-foreground">{t(($) => $.owner_only)}</p>
        )}
      </section>

      {adding && (
        <AddServerDialog
          onClose={() => setAdding(false)}
          onSave={(input) =>
            create.mutateAsync(input).then((server) => {
              toast.success(t(($) => $.added_toast, { name: server.name }));
            })
          }
        />
      )}

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && !remove.isPending && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.delete_title, { name: deleting?.name ?? "" })}</AlertDialogTitle>
            <AlertDialogDescription>{t(($) => $.delete_description)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={remove.isPending}>{t(($) => $.cancel)}</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleDelete} disabled={remove.isPending}>
              {remove.isPending && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
              {t(($) => $.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function Notice({ text, loading = false }: { text: string; loading?: boolean }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border border-dashed px-4 py-6 text-caption text-muted-foreground">
      {loading ? (
        <Loader2 className="size-4 animate-spin motion-reduce:animate-none" aria-hidden="true" />
      ) : (
        <Server className="size-4" aria-hidden="true" />
      )}
      {text}
    </div>
  );
}

function AddServerDialog({
  onClose,
  onSave,
}: {
  onClose: () => void;
  onSave: (input: { name: string; url: string; scope: string; extraHeaders: Record<string, string> }) => Promise<void>;
}) {
  const { t } = useT("mcpoauth");
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [scope, setScope] = useState("");
  const [headers, setHeaders] = useState("");
  const [headersInvalid, setHeadersInvalid] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const submit = () => {
    const extraHeaders = parseExtraHeaders(headers);
    setHeadersInvalid(extraHeaders === null);
    if (extraHeaders === null) return;
    setSaving(true);
    setError("");
    onSave({ name: name.trim(), url: url.trim(), scope: scope.trim(), extraHeaders }).then(
      () => {
        setSaving(false);
        onClose();
      },
      (err) => {
        setSaving(false);
        // Keep the input; say what the server refused.
        setError(errorMessage(err, t(($) => $.action_failed)));
      },
    );
  };

  return (
    <Dialog open onOpenChange={(open) => !open && !saving && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.dialog.title)}</DialogTitle>
        </DialogHeader>
        <form
          noValidate
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
        >
          <FieldGroup className="gap-6">
            <Field>
              <FieldLabel htmlFor="mcp-oauth-name">{t(($) => $.dialog.name_label)}</FieldLabel>
              <Input
                id="mcp-oauth-name"
                autoFocus
                required
                autoComplete="off"
                spellCheck={false}
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
              <FieldDescription className="text-caption">{t(($) => $.dialog.name_hint)}</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="mcp-oauth-url">{t(($) => $.dialog.url_label)}</FieldLabel>
              <Input
                id="mcp-oauth-url"
                type="url"
                required
                autoComplete="off"
                spellCheck={false}
                placeholder="https://"
                value={url}
                onChange={(event) => setUrl(event.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="mcp-oauth-scope">{t(($) => $.dialog.scope_label)}</FieldLabel>
              <Input
                id="mcp-oauth-scope"
                autoComplete="off"
                spellCheck={false}
                value={scope}
                onChange={(event) => setScope(event.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="mcp-oauth-headers">{t(($) => $.dialog.headers_label)}</FieldLabel>
              <Textarea
                id="mcp-oauth-headers"
                rows={3}
                autoComplete="off"
                spellCheck={false}
                value={headers}
                onChange={(event) => {
                  setHeaders(event.target.value);
                  setHeadersInvalid(false);
                }}
                aria-invalid={headersInvalid || undefined}
              />
              {headersInvalid ? (
                <FieldError className="text-caption">{t(($) => $.dialog.headers_invalid)}</FieldError>
              ) : (
                <FieldDescription className="text-caption">{t(($) => $.dialog.headers_hint)}</FieldDescription>
              )}
            </Field>
            {error && (
              <p role="alert" className="text-caption text-destructive">
                {error}
              </p>
            )}
          </FieldGroup>
          <DialogFooter className="mt-6">
            <Button type="button" variant="outline" disabled={saving} onClick={onClose}>
              {t(($) => $.cancel)}
            </Button>
            <Button type="submit" disabled={saving || name.trim() === "" || url.trim() === ""} aria-busy={saving || undefined}>
              {saving && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
              {t(($) => $.dialog.save)}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
