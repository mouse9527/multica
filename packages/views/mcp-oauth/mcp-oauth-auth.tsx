"use client";

// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

import { useId } from "react";
import { MCP_OAUTH_AUTH_TYPE } from "@multica/core/mcp-oauth";
import { cn } from "@multica/ui/lib/utils";
import { Field, FieldDescription, FieldLabel } from "@multica/ui/components/ui/field";
import { Input } from "@multica/ui/components/ui/input";
import { useT } from "../i18n";

/** How an HTTP MCP entry authenticates, as the entry dialog offers it. */
export type McpFormAuth = "none" | "headers" | "oauth";

const AUTH_OPTIONS: McpFormAuth[] = ["none", "headers", "oauth"];

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

/**
 * The OAuth settings of a saved entry, or null when it does not ask for OAuth
 * (`"auth": {"type": "oauth", "scope": "..."}`). Any other `auth` value is not
 * the form's to edit and stays where it is.
 */
export function oauthFormAuthFromConfig(config: Record<string, unknown>): { scope: string } | null {
  const auth = config.auth;
  if (!isRecord(auth) || auth.type !== MCP_OAUTH_AUTH_TYPE) return null;
  return { scope: typeof auth.scope === "string" ? auth.scope : "" };
}

/**
 * The "Authentication" choice of an HTTP entry: none, fixed headers (edited by
 * the dialog's own header editor), or OAuth 2.1 with an optional scope. An
 * OAuth entry holds no credential: it is connected from the server list once
 * saved.
 */
export function McpOAuthAuthPicker({
  value,
  scope,
  onChange,
  onScopeChange,
}: {
  value: McpFormAuth;
  scope: string;
  onChange: (value: McpFormAuth) => void;
  onScopeChange: (scope: string) => void;
}) {
  const { t } = useT("mcpoauth");
  const scopeId = useId();
  return (
    <div className="space-y-4">
      <fieldset className="space-y-3">
        <legend className="text-body font-medium">{t(($) => $.auth.label)}</legend>
        <div className="grid grid-cols-3 gap-2">
          {AUTH_OPTIONS.map((option) => {
            const selected = value === option;
            return (
              <button
                key={option}
                type="button"
                aria-pressed={selected}
                className={cn(
                  "min-h-9 rounded-md border px-3 py-2 text-body transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  selected
                    ? "border-primary bg-accent/50 font-medium text-foreground"
                    : "border-surface-border text-muted-foreground hover:bg-accent/30",
                )}
                onClick={() => onChange(option)}
              >
                {t(($) => $.auth[option])}
              </button>
            );
          })}
        </div>
      </fieldset>
      {value === "oauth" ? (
        <Field>
          <FieldLabel htmlFor={scopeId}>{t(($) => $.auth.scope_label)}</FieldLabel>
          <Input
            id={scopeId}
            name="mcp-server-oauth-scope"
            autoComplete="off"
            spellCheck={false}
            value={scope}
            onChange={(event) => onScopeChange(event.target.value)}
          />
          <FieldDescription className="text-caption">{t(($) => $.auth.oauth_hint)}</FieldDescription>
        </Field>
      ) : null}
    </div>
  );
}
