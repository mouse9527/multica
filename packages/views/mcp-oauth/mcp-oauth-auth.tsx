"use client";

// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: pre-registered OAuth clients (M-295).
// Modified for the mouse9527/multica fork: manual endpoints, resource and audience (M-295 stage 2).
// Modified for the mouse9527/multica fork: extra authorization parameters (M-295 stage 3).

import { useId, useState, type ReactNode } from "react";
import {
  MCP_OAUTH_AUTH_TYPE,
  MCP_OAUTH_AUTHORIZATION_PARAMS,
  MCP_OAUTH_TOKEN_AUTH_METHODS,
  type McpOAuthAuthorizationParam,
  type McpOAuthTokenAuthMethod,
} from "@multica/core/mcp-oauth";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@multica/ui/components/ui/collapsible";
import { ChevronRight } from "@multica/ui/icons";
import { Field, FieldDescription, FieldError, FieldLabel } from "@multica/ui/components/ui/field";
import { Input } from "@multica/ui/components/ui/input";
import { useT } from "../i18n";

/** How an HTTP MCP entry authenticates, as the entry dialog offers it. */
export type McpFormAuth = "none" | "headers" | "oauth";

const AUTH_OPTIONS: McpFormAuth[] = ["none", "headers", "oauth"];

/** How the connect gets its OAuth client: registered on the fly (RFC 7591), or registered in advance. */
export type McpOAuthClientMode = "dynamic" | "preregistered";

const CLIENT_MODES: McpOAuthClientMode[] = ["dynamic", "preregistered"];

/** The token endpoint auth method, or "" to let the server pick from the client secret and discovery. */
export type McpOAuthTokenAuthChoice = "" | McpOAuthTokenAuthMethod;

const TOKEN_AUTH_CHOICES: McpOAuthTokenAuthChoice[] = ["", ...MCP_OAUTH_TOKEN_AUTH_METHODS];

/** The OAuth settings the entry dialog edits. */
export type McpOAuthFormValue = {
  scope: string;
  clientMode: McpOAuthClientMode;
  clientId: string;
  issuer: string;
  tokenAuthMethod: McpOAuthTokenAuthChoice;
  /**
   * A client secret typed into this dialog, saved through its own endpoint
   * after the entry. Never prefilled: the server does not return it.
   */
  clientSecret: string;
  /** Remove the stored client secret when saving. */
  clearClientSecret: boolean;
  // Advanced: where the authorization server is, and what the token is for.
  authorizationEndpoint: string;
  tokenEndpoint: string;
  resource: string;
  audience: string;
  /** Extra authorization request parameters, by allowlisted key; "" is unset. */
  authorizationParams: Partial<Record<McpOAuthAuthorizationParam, string>>;
};

export const emptyOAuthForm = (): McpOAuthFormValue => ({
  scope: "",
  clientMode: "dynamic",
  clientId: "",
  issuer: "",
  tokenAuthMethod: "",
  clientSecret: "",
  clearClientSecret: false,
  authorizationEndpoint: "",
  tokenEndpoint: "",
  resource: "",
  audience: "",
  authorizationParams: {},
});

/** Whether any advanced setting is filled in, so the section opens on them. */
export function hasAdvancedOAuthSettings(value: McpOAuthFormValue): boolean {
  return (
    value.authorizationEndpoint !== "" ||
    value.tokenEndpoint !== "" ||
    value.resource !== "" ||
    value.audience !== "" ||
    Object.values(value.authorizationParams).some((param) => (param ?? "") !== "") ||
    (value.clientMode === "dynamic" && value.issuer !== "")
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

function stringField(record: Record<string, unknown>, key: string): string {
  const value = record[key];
  return typeof value === "string" ? value : "";
}

/**
 * The OAuth settings of a saved entry, or null when it does not ask for OAuth
 * (`"auth": {"type": "oauth", "scope": "...", "client_id": "...", ...}`). Any
 * other `auth` value is not the form's to edit and stays where it is.
 */
export function oauthFormAuthFromConfig(config: Record<string, unknown>): McpOAuthFormValue | null {
  const auth = config.auth;
  if (!isRecord(auth) || auth.type !== MCP_OAUTH_AUTH_TYPE) return null;
  const clientId = stringField(auth, "client_id");
  const method = stringField(auth, "token_endpoint_auth_method");
  return {
    ...emptyOAuthForm(),
    scope: stringField(auth, "scope"),
    clientMode: clientId ? "preregistered" : "dynamic",
    clientId,
    issuer: stringField(auth, "issuer"),
    tokenAuthMethod: (MCP_OAUTH_TOKEN_AUTH_METHODS as readonly string[]).includes(method)
      ? (method as McpOAuthTokenAuthMethod)
      : "",
    authorizationEndpoint: stringField(auth, "authorization_endpoint"),
    tokenEndpoint: stringField(auth, "token_endpoint"),
    resource: stringField(auth, "resource"),
    audience: stringField(auth, "audience"),
    authorizationParams: authorizationParamsFrom(auth.authorization_params),
  };
}

/**
 * The entry's `auth` object. An entry that registers its client on the fly
 * keeps exactly the shape it had before pre-registered clients existed.
 */
export function oauthAuthFromForm(value: McpOAuthFormValue): Record<string, unknown> {
  const auth: Record<string, unknown> = { type: MCP_OAUTH_AUTH_TYPE };
  const scope = value.scope.trim();
  if (scope) auth.scope = scope;
  const issuer = value.issuer.trim();
  if (value.clientMode === "preregistered") {
    auth.client_id = value.clientId.trim();
    auth.issuer = issuer;
    if (value.tokenAuthMethod) auth.token_endpoint_auth_method = value.tokenAuthMethod;
  } else if (issuer) {
    auth.issuer = issuer;
  }
  const optional: [string, string][] = [
    ["authorization_endpoint", value.authorizationEndpoint],
    ["token_endpoint", value.tokenEndpoint],
    ["resource", value.resource],
    ["audience", value.audience],
  ];
  for (const [key, raw] of optional) {
    const text = raw.trim();
    if (text) auth[key] = text;
  }
  const params: Record<string, string> = {};
  for (const key of MCP_OAUTH_AUTHORIZATION_PARAMS) {
    const text = (value.authorizationParams[key] ?? "").trim();
    if (text) params[key] = text;
  }
  if (Object.keys(params).length > 0) auth.authorization_params = params;
  return auth;
}

/** The allowlisted string values of a saved `authorization_params`. */
function authorizationParamsFrom(raw: unknown): Partial<Record<McpOAuthAuthorizationParam, string>> {
  if (!isRecord(raw)) return {};
  const out: Partial<Record<McpOAuthAuthorizationParam, string>> = {};
  for (const key of MCP_OAUTH_AUTHORIZATION_PARAMS) {
    const value = raw[key];
    if (typeof value === "string") out[key] = value;
  }
  return out;
}

/** What blocks saving the OAuth settings, if anything. */
export type McpOAuthFormError =
  | "client_id"
  | "issuer"
  | "endpoints_pair"
  | "endpoints_need_client"
  | "authorization_endpoint"
  | "token_endpoint";

const HTTPS_URL = /^https:\/\/[^\s#]+$/i;

export function oauthFormError(value: McpOAuthFormValue): McpOAuthFormError | null {
  const preregistered = value.clientMode === "preregistered";
  const issuer = value.issuer.trim();
  if (preregistered && value.clientId.trim() === "") return "client_id";
  if ((preregistered || issuer !== "") && !HTTPS_URL.test(issuer)) return "issuer";
  const authorizationEndpoint = value.authorizationEndpoint.trim();
  const tokenEndpoint = value.tokenEndpoint.trim();
  if ((authorizationEndpoint === "") !== (tokenEndpoint === "")) return "endpoints_pair";
  if (authorizationEndpoint === "") return null;
  if (!preregistered) return "endpoints_need_client";
  if (!HTTPS_URL.test(authorizationEndpoint)) return "authorization_endpoint";
  if (!HTTPS_URL.test(tokenEndpoint)) return "token_endpoint";
  return null;
}

/**
 * The client secret change to save after the entry: a new secret, null to
 * remove the stored one, or undefined for none.
 */
export function oauthClientSecretChange(value: McpOAuthFormValue): string | null | undefined {
  if (value.clientMode !== "preregistered") return undefined;
  if (value.clientSecret !== "") return value.clientSecret;
  return value.clearClientSecret ? null : undefined;
}

function Choice<T extends string>({
  legend,
  options,
  value,
  label,
  columns,
  onChange,
}: {
  legend: string;
  options: T[];
  value: T;
  label: (option: T) => string;
  columns: string;
  onChange: (value: T) => void;
}) {
  return (
    <fieldset className="space-y-3">
      <legend className="text-body font-medium">{legend}</legend>
      <div className={cn("grid gap-2", columns)}>
        {options.map((option) => {
          const selected = value === option;
          return (
            <button
              key={option || "default"}
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
              {label(option)}
            </button>
          );
        })}
      </div>
    </fieldset>
  );
}

/**
 * The "Authentication" choice of an HTTP entry: none, fixed headers (edited by
 * the dialog's own header editor), or OAuth 2.1 with an optional scope and
 * either a client registered on the fly or one registered in advance. An
 * OAuth entry holds no credential: its client secret is saved on its own and
 * never shown again, and it is connected from the server list once saved.
 */
export function McpOAuthAuthPicker({
  value,
  oauth,
  error = null,
  clientSecretSet = false,
  clientSecretEditable = false,
  onChange,
  onOAuthChange,
}: {
  value: McpFormAuth;
  oauth: McpOAuthFormValue;
  /** Shown once the user tried to save. */
  error?: McpOAuthFormError | null;
  /** A client secret is stored for the saved entry. */
  clientSecretSet?: boolean;
  /** Whether this dialog can save a client secret. */
  clientSecretEditable?: boolean;
  onChange: (value: McpFormAuth) => void;
  onOAuthChange: (patch: Partial<McpOAuthFormValue>) => void;
}) {
  const { t } = useT("mcpoauth");
  const scopeId = useId();
  const clientIdId = useId();
  const issuerId = useId();
  const secretId = useId();
  const clientIdErrorId = `${clientIdId}-error`;
  const issuerErrorId = `${issuerId}-error`;
  const secretHintId = `${secretId}-hint`;
  const secretKept = clientSecretSet && !oauth.clearClientSecret && oauth.clientSecret === "";
  const preregistered = oauth.clientMode === "preregistered";
  const [advancedOpen, setAdvancedOpen] = useState(() => hasAdvancedOAuthSettings(oauth));
  // Stage 2 (M-295): the issuer is required for a pre-registered client and
  // shown with it; for automatic registration it is optional and advanced.
  const issuerField = (
    <Field>
      <FieldLabel htmlFor={issuerId}>
        {preregistered ? t(($) => $.client.issuer_label) : t(($) => $.advanced.issuer_label)}
      </FieldLabel>
      <Input
        id={issuerId}
        name="mcp-server-oauth-issuer"
        placeholder="https://auth.example.com"
        type="url"
        inputMode="url"
        autoComplete="off"
        spellCheck={false}
        value={oauth.issuer}
        onChange={(event) => onOAuthChange({ issuer: event.target.value })}
        aria-invalid={error === "issuer" ? true : undefined}
        aria-describedby={error === "issuer" ? issuerErrorId : undefined}
        className="aria-invalid:border-input aria-invalid:ring-0"
      />
      {error === "issuer" ? (
        <FieldError id={issuerErrorId} className="text-caption">
          {t(($) => $.client.issuer_required)}
        </FieldError>
      ) : (
        <FieldDescription className="text-caption">
          {preregistered ? t(($) => $.client.issuer_hint) : t(($) => $.advanced.issuer_hint)}
        </FieldDescription>
      )}
    </Field>
  );
  const advancedError =
    error === "endpoints_pair" ||
    error === "endpoints_need_client" ||
    error === "authorization_endpoint" ||
    error === "token_endpoint" ||
    (error === "issuer" && !preregistered);
  return (
    <div className="space-y-4">
      <Choice
        legend={t(($) => $.auth.label)}
        options={AUTH_OPTIONS}
        value={value}
        label={(option) => t(($) => $.auth[option])}
        columns="grid-cols-3"
        onChange={onChange}
      />
      {value === "oauth" ? (
        <>
          <Field>
            <FieldLabel htmlFor={scopeId}>{t(($) => $.auth.scope_label)}</FieldLabel>
            <Input
              id={scopeId}
              name="mcp-server-oauth-scope"
              autoComplete="off"
              spellCheck={false}
              value={oauth.scope}
              onChange={(event) => onOAuthChange({ scope: event.target.value })}
            />
            <FieldDescription className="text-caption">{t(($) => $.auth.oauth_hint)}</FieldDescription>
          </Field>
          <Choice
            legend={t(($) => $.client.label)}
            options={CLIENT_MODES}
            value={oauth.clientMode}
            label={(option) => t(($) => $.client[option])}
            columns="grid-cols-2"
            onChange={(clientMode) => {
              // Back to automatic registration, a pre-registered client's
              // issuer stays as an advanced setting; show it there rather
              // than keep it folded away.
              if (clientMode === "dynamic" && oauth.issuer.trim() !== "") setAdvancedOpen(true);
              onOAuthChange({ clientMode });
            }}
          />
          {oauth.clientMode === "preregistered" ? (
            <>
              <Field>
                <FieldLabel htmlFor={clientIdId}>{t(($) => $.client.client_id_label)}</FieldLabel>
                <Input
                  id={clientIdId}
                  name="mcp-server-oauth-client-id"
                  autoComplete="off"
                  spellCheck={false}
                  value={oauth.clientId}
                  onChange={(event) => onOAuthChange({ clientId: event.target.value })}
                  aria-invalid={error === "client_id" ? true : undefined}
                  aria-describedby={error === "client_id" ? clientIdErrorId : undefined}
                  className="aria-invalid:border-input aria-invalid:ring-0"
                />
                {error === "client_id" ? (
                  <FieldError id={clientIdErrorId} className="text-caption">
                    {t(($) => $.client.client_id_required)}
                  </FieldError>
                ) : null}
              </Field>
              {issuerField}
              {clientSecretEditable ? (
                <Field>
                  <FieldLabel htmlFor={secretId}>{t(($) => $.client.secret_label)}</FieldLabel>
                  <Input
                    id={secretId}
                    name="mcp-server-oauth-client-secret"
                    type="password"
                    autoComplete="new-password"
                    spellCheck={false}
                    placeholder={secretKept ? t(($) => $.client.secret_replace_placeholder) : undefined}
                    value={oauth.clientSecret}
                    onChange={(event) =>
                      onOAuthChange({ clientSecret: event.target.value, clearClientSecret: false })
                    }
                    aria-describedby={secretHintId}
                  />
                  <div className="flex items-center justify-between gap-3">
                    <FieldDescription id={secretHintId} className="text-caption">
                      {oauth.clearClientSecret
                        ? t(($) => $.client.secret_cleared)
                        : secretKept
                          ? t(($) => $.client.secret_set)
                          : t(($) => $.client.secret_hint)}
                    </FieldDescription>
                    {clientSecretSet && !oauth.clearClientSecret ? (
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() => onOAuthChange({ clientSecret: "", clearClientSecret: true })}
                      >
                        {t(($) => $.client.secret_remove)}
                      </Button>
                    ) : null}
                  </div>
                </Field>
              ) : null}
              <Choice
                legend={t(($) => $.client.method_label)}
                options={TOKEN_AUTH_CHOICES}
                value={oauth.tokenAuthMethod}
                label={(option) => t(($) => $.client.method[option || "auto"])}
                columns="grid-cols-2"
                onChange={(tokenAuthMethod) => onOAuthChange({ tokenAuthMethod })}
              />
              {oauth.authorizationEndpoint.trim() !== "" ? (
                // Without discovery the server's accepted methods are unknown.
                <p className="text-caption text-muted-foreground">{t(($) => $.client.method_manual_hint)}</p>
              ) : null}
            </>
          ) : null}
          <McpOAuthAdvanced
            oauth={oauth}
            error={error}
            open={advancedOpen || advancedError}
            onOpenChange={setAdvancedOpen}
            issuerField={preregistered ? null : issuerField}
            onOAuthChange={onOAuthChange}
          />
        </>
      ) : null}
    </div>
  );
}

/**
 * The advanced OAuth settings, folded away: where the authorization server
 * is (the issuer for automatic registration; manual endpoints, which skip
 * discovery and need a pre-registered client) and what the token is for
 * (resource indicator, audience). The picker opens it when any is set or has
 * an error.
 */
function McpOAuthAdvanced({
  oauth,
  error,
  open,
  onOpenChange,
  issuerField,
  onOAuthChange,
}: {
  oauth: McpOAuthFormValue;
  error: McpOAuthFormError | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  issuerField: ReactNode;
  onOAuthChange: (patch: Partial<McpOAuthFormValue>) => void;
}) {
  const { t } = useT("mcpoauth");
  const authorizationId = useId();
  const tokenId = useId();
  const resourceId = useId();
  const audienceId = useId();
  const authorizationErrorId = `${authorizationId}-error`;
  const tokenErrorId = `${tokenId}-error`;
  const endpointsError =
    error === "endpoints_pair"
      ? t(($) => $.advanced.endpoints_pair)
      : error === "endpoints_need_client"
        ? t(($) => $.advanced.endpoints_need_client)
        : error === "token_endpoint"
          ? t(($) => $.advanced.https_required)
          : "";
  const textField = (
    id: string,
    name: string,
    label: string,
    value: string,
    onValue: (value: string) => void,
    options: { placeholder?: string; hint?: string; errorId?: string; error?: string } = {},
  ) => (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input
        id={id}
        name={name}
        placeholder={options.placeholder}
        autoComplete="off"
        spellCheck={false}
        value={value}
        onChange={(event) => onValue(event.target.value)}
        aria-invalid={options.error ? true : undefined}
        aria-describedby={options.error ? options.errorId : undefined}
        className="aria-invalid:border-input aria-invalid:ring-0"
      />
      {options.error ? (
        <FieldError id={options.errorId} className="text-caption">
          {options.error}
        </FieldError>
      ) : options.hint ? (
        <FieldDescription className="text-caption">{options.hint}</FieldDescription>
      ) : null}
    </Field>
  );
  return (
    <Collapsible open={open} onOpenChange={(next) => onOpenChange(next)}>
      <CollapsibleTrigger
        render={
          <button
            type="button"
            className="group flex items-center gap-1 rounded-md text-body font-medium text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
          />
        }
      >
        <ChevronRight
          aria-hidden
          className="size-4 transition-transform group-aria-expanded:rotate-90 motion-reduce:transition-none"
        />
        {t(($) => $.advanced.label)}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="space-y-4 pt-4">
          {issuerField}
          {textField(
            authorizationId,
            "mcp-server-oauth-authorization-endpoint",
            t(($) => $.advanced.authorization_endpoint_label),
            oauth.authorizationEndpoint,
            (authorizationEndpoint) => onOAuthChange({ authorizationEndpoint }),
            {
              placeholder: "https://auth.example.com/authorize",
              errorId: authorizationErrorId,
              error: error === "authorization_endpoint" ? t(($) => $.advanced.https_required) : undefined,
            },
          )}
          {textField(
            tokenId,
            "mcp-server-oauth-token-endpoint",
            t(($) => $.advanced.token_endpoint_label),
            oauth.tokenEndpoint,
            (tokenEndpoint) => onOAuthChange({ tokenEndpoint }),
            {
              placeholder: "https://auth.example.com/token",
              hint: t(($) => $.advanced.endpoints_hint),
              errorId: tokenErrorId,
              error: endpointsError || undefined,
            },
          )}
          {textField(
            resourceId,
            "mcp-server-oauth-resource",
            t(($) => $.advanced.resource_label),
            oauth.resource,
            (resource) => onOAuthChange({ resource }),
            { hint: t(($) => $.advanced.resource_hint) },
          )}
          {textField(
            audienceId,
            "mcp-server-oauth-audience",
            t(($) => $.advanced.audience_label),
            oauth.audience,
            (audience) => onOAuthChange({ audience }),
            { hint: t(($) => $.advanced.audience_hint) },
          )}
          <fieldset className="space-y-3">
            <legend className="text-body font-medium">{t(($) => $.advanced.params_label)}</legend>
            <p className="text-caption text-muted-foreground">{t(($) => $.advanced.params_hint)}</p>
            <div className="grid gap-3 sm:grid-cols-2">
              {MCP_OAUTH_AUTHORIZATION_PARAMS.map((key) => (
                <McpOAuthParamField
                  key={key}
                  name={key}
                  value={oauth.authorizationParams[key] ?? ""}
                  onChange={(value) =>
                    onOAuthChange({ authorizationParams: { ...oauth.authorizationParams, [key]: value } })
                  }
                />
              ))}
            </div>
          </fieldset>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

/** One extra authorization parameter; its label is the parameter's own name. */
function McpOAuthParamField({
  name,
  value,
  onChange,
}: {
  name: McpOAuthAuthorizationParam;
  value: string;
  onChange: (value: string) => void;
}) {
  const id = useId();
  return (
    <Field>
      <FieldLabel htmlFor={id} className="font-mono">
        {name}
      </FieldLabel>
      <Input
        id={id}
        name={`mcp-server-oauth-param-${name}`}
        autoComplete="off"
        spellCheck={false}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </Field>
  );
}

/**
 * Shown by the entry dialog once the entry is saved but its client secret is
 * not: saving again retries only the secret. `changed`: the entry was edited
 * since, which this dialog cannot save again without adding it twice.
 */
export function McpOAuthSavedEntryNote({ changed }: { changed: boolean }) {
  const { t } = useT("mcpoauth");
  return (
    <p
      role={changed ? "alert" : "status"}
      className={cn("mr-auto self-center text-caption", changed ? "text-destructive" : "text-muted-foreground")}
    >
      {changed ? t(($) => $.client.saved_entry_changed) : t(($) => $.client.secret_retry)}
    </p>
  );
}
