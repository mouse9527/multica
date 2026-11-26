// @vitest-environment jsdom
// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).
// Modified for the mouse9527/multica fork: pre-registered OAuth clients (M-295).

import type { ComponentProps, ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../../locales/en/common.json";
import enAgents from "../../../locales/en/agents.json";
import enMcpOAuth from "../../../locales/en/mcpoauth.json";
import type { ManagedMcpServer } from "./mcp-config-model";
import { McpServerDialog } from "./mcp-server-dialog";

const TEST_RESOURCES = { en: { common: enCommon, agents: enAgents, mcpoauth: enMcpOAuth } };

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

const OAUTH_ENTRY = {
  type: "http",
  url: "https://mcp.example.com/mcp",
  auth: { type: "oauth", scope: "mcp read" },
};

function httpServer(config: Record<string, unknown>): ManagedMcpServer {
  return { name: "gateway", config, container: "mcpServers", transport: "http", enabled: true };
}

function renderDialog(overrides: Partial<ComponentProps<typeof McpServerDialog>> = {}) {
  const onSave = vi.fn().mockResolvedValue(undefined);
  const props = {
    open: true,
    server: null,
    existingNames: new Set<string>(),
    onSave,
    onOpenChange: vi.fn(),
    ...overrides,
  };
  render(<McpServerDialog {...props} />, { wrapper: Wrapper });
  return { onSave };
}

describe("McpServerDialog authentication", () => {
  // The first thing to settle (M-269): the guided form must not drop an
  // entry's `auth` when it saves. With OAuth not offered it is an unknown
  // field the form carries through untouched, as upstream.
  it("keeps an OAuth entry's auth through the visual editor when OAuth is not offered", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ server: httpServer(OAUTH_ENTRY), hideNameWhenEditing: true });

    expect(screen.queryByRole("button", { name: "OAuth 2.1" })).toBeNull();
    const url = screen.getByLabelText("Server URL");
    await user.clear(url);
    await user.type(url, "https://mcp.example.com/v2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(onSave).toHaveBeenCalledWith("gateway", { ...OAUTH_ENTRY, url: "https://mcp.example.com/v2" }),
    );
  });

  it("opens an OAuth entry on OAuth with its scope, and saves it back without headers", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({
      server: httpServer(OAUTH_ENTRY),
      hideNameWhenEditing: true,
      oauthAvailable: true,
    });

    expect(screen.getByRole("button", { name: "OAuth 2.1" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByLabelText("Scope (optional)")).toHaveValue("mcp read");
    expect(screen.queryByRole("button", { name: "Add header" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(onSave).toHaveBeenCalledWith("gateway", OAUTH_ENTRY));
  });

  it("creates an OAuth entry from the form", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ oauthAvailable: true });

    await user.type(screen.getByLabelText("Server name"), "gateway");
    await user.click(screen.getByRole("button", { name: /^Streamable HTTP/ }));
    await user.type(screen.getByLabelText("Server URL"), "https://mcp.example.com/mcp");
    expect(screen.getByRole("button", { name: "None" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    expect(screen.getByText("After saving, connect it from the server list.")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Add" }));

    await waitFor(() =>
      expect(onSave).toHaveBeenCalledWith("gateway", {
        type: "http",
        url: "https://mcp.example.com/mcp",
        auth: { type: "oauth" },
      }),
    );
  });

  it("asks for an HTTPS URL before saving an OAuth entry", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ oauthAvailable: true });

    await user.type(screen.getByLabelText("Server name"), "gateway");
    await user.click(screen.getByRole("button", { name: /^Streamable HTTP/ }));
    await user.type(screen.getByLabelText("Server URL"), "http://mcp.example.com/mcp");
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    await user.click(screen.getByRole("button", { name: "Add" }));

    expect(await screen.findByText("OAuth needs an HTTPS URL.")).toBeTruthy();
    expect(screen.getByLabelText("Server URL")).toHaveFocus();
    expect(onSave).not.toHaveBeenCalled();
  });

  it("writes fixed headers only when they are the chosen authentication", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({
      server: httpServer({ type: "http", url: "https://mcp.example.com/mcp", headers: { "x-api-key": "k" } }),
      hideNameWhenEditing: true,
      oauthAvailable: true,
    });

    expect(screen.getByRole("button", { name: "Fixed headers" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Add header" })).toBeTruthy();
    // Switching to OAuth leaves the static key behind: an OAuth entry carries
    // no credential of its own.
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(onSave).toHaveBeenCalledWith("gateway", {
        type: "http",
        url: "https://mcp.example.com/mcp",
        auth: { type: "oauth" },
      }),
    );
  });

  it("does not offer authentication for a command", () => {
    renderDialog({ oauthAvailable: true });
    expect(screen.queryByRole("button", { name: "OAuth 2.1" })).toBeNull();
  });
});

const PREREGISTERED_ENTRY = {
  type: "http",
  url: "https://mcp.example.com/mcp",
  auth: { type: "oauth", client_id: "pre-1", issuer: "https://auth.example.com", token_endpoint_auth_method: "client_secret_post" },
};

describe("McpServerDialog pre-registered OAuth client", () => {
  it("creates a pre-registered entry, then saves its secret on its own", async () => {
    const user = userEvent.setup();
    const calls: string[] = [];
    const onSave = vi.fn(async () => {
      calls.push("entry");
    });
    const onSaveOAuthClientSecret = vi.fn(async () => {
      calls.push("secret");
    });
    renderDialog({ oauthAvailable: true, onSave, onSaveOAuthClientSecret });

    await user.type(screen.getByLabelText("Server name"), "gateway");
    await user.click(screen.getByRole("button", { name: /^Streamable HTTP/ }));
    await user.type(screen.getByLabelText("Server URL"), "https://mcp.example.com/mcp");
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    await user.click(screen.getByRole("button", { name: "Pre-registered" }));
    await user.type(screen.getByLabelText("Client ID"), "pre-1");
    await user.type(screen.getByLabelText("Issuer"), "https://auth.example.com");
    const secret = screen.getByLabelText("Client secret (optional)");
    expect(secret).toHaveAttribute("type", "password");
    await user.type(secret, "sk-typed");
    await user.click(screen.getByRole("button", { name: "client_secret_basic" }));
    await user.click(screen.getByRole("button", { name: "Add" }));

    await waitFor(() => expect(onSaveOAuthClientSecret).toHaveBeenCalledWith("gateway", "sk-typed"));
    // The secret never goes into the entry, and is saved after it.
    expect(onSave).toHaveBeenCalledWith("gateway", {
      type: "http",
      url: "https://mcp.example.com/mcp",
      auth: { type: "oauth", client_id: "pre-1", issuer: "https://auth.example.com", token_endpoint_auth_method: "client_secret_basic" },
    });
    expect(calls).toEqual(["entry", "secret"]);
  });

  it("opens a saved client without its secret and keeps the secret unless replaced", async () => {
    const user = userEvent.setup();
    const onSaveOAuthClientSecret = vi.fn().mockResolvedValue(undefined);
    const { onSave } = renderDialog({
      server: httpServer(PREREGISTERED_ENTRY),
      hideNameWhenEditing: true,
      oauthAvailable: true,
      oauthClientSecretSet: true,
      onSaveOAuthClientSecret,
    });

    expect(screen.getByRole("button", { name: "Pre-registered" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByLabelText("Client ID")).toHaveValue("pre-1");
    expect(screen.getByLabelText("Issuer")).toHaveValue("https://auth.example.com");
    expect(screen.getByLabelText("Client secret (optional)")).toHaveValue("");
    expect(screen.getByText("A client secret is set.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "client_secret_post" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(onSave).toHaveBeenCalledWith("gateway", PREREGISTERED_ENTRY));
    expect(onSaveOAuthClientSecret).not.toHaveBeenCalled();
  });

  it("removes a stored secret when asked", async () => {
    const user = userEvent.setup();
    const onSaveOAuthClientSecret = vi.fn().mockResolvedValue(undefined);
    renderDialog({
      server: httpServer(PREREGISTERED_ENTRY),
      hideNameWhenEditing: true,
      oauthAvailable: true,
      oauthClientSecretSet: true,
      onSaveOAuthClientSecret,
    });

    await user.click(screen.getByRole("button", { name: "Remove" }));
    expect(screen.getByText("The client secret will be removed when you save.")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSaveOAuthClientSecret).toHaveBeenCalledWith("gateway", null));
  });

  // Review case (M-295 R1): the entry is added, its secret is not. With the
  // name now taken, saving again must retry only the secret.
  it("retries only the secret when it failed after the entry was added", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn().mockResolvedValue(undefined);
    const onSaveOAuthClientSecret = vi.fn().mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce(undefined);
    const onOpenChange = vi.fn();
    const existingNames = new Set<string>();
    const view = render(
      <McpServerDialog
        open
        server={null}
        existingNames={existingNames}
        oauthAvailable
        onSave={onSave}
        onSaveOAuthClientSecret={onSaveOAuthClientSecret}
        onOpenChange={onOpenChange}
      />,
      { wrapper: Wrapper },
    );

    await user.type(screen.getByLabelText("Server name"), "gateway");
    await user.click(screen.getByRole("button", { name: /^Streamable HTTP/ }));
    await user.type(screen.getByLabelText("Server URL"), "https://mcp.example.com/mcp");
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    await user.click(screen.getByRole("button", { name: "Pre-registered" }));
    await user.type(screen.getByLabelText("Client ID"), "pre-1");
    await user.type(screen.getByLabelText("Issuer"), "https://auth.example.com");
    await user.type(screen.getByLabelText("Client secret (optional)"), "sk-1");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(await screen.findByText("The server is saved. Save again to retry the client secret.")).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);

    // The list now has the entry: its name must not read as a duplicate.
    view.rerender(
      <McpServerDialog
        open
        server={null}
        existingNames={new Set(["gateway"])}
        oauthAvailable
        onSave={onSave}
        onSaveOAuthClientSecret={onSaveOAuthClientSecret}
        onOpenChange={onOpenChange}
      />,
    );
    const secret = screen.getByLabelText("Client secret (optional)");
    await user.clear(secret);
    await user.type(secret, "sk-2");
    await user.click(screen.getByRole("button", { name: "Add" }));

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onSaveOAuthClientSecret).toHaveBeenLastCalledWith("gateway", "sk-2");
  });

  it("does not add an entry twice when it was changed after being added", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn().mockResolvedValue(undefined);
    const onSaveOAuthClientSecret = vi.fn().mockRejectedValue(new Error("offline"));
    renderDialog({ oauthAvailable: true, onSave, onSaveOAuthClientSecret });

    await user.type(screen.getByLabelText("Server name"), "gateway");
    await user.click(screen.getByRole("button", { name: /^Streamable HTTP/ }));
    await user.type(screen.getByLabelText("Server URL"), "https://mcp.example.com/mcp");
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    await user.click(screen.getByRole("button", { name: "Pre-registered" }));
    await user.type(screen.getByLabelText("Client ID"), "pre-1");
    await user.type(screen.getByLabelText("Issuer"), "https://auth.example.com");
    await user.type(screen.getByLabelText("Client secret (optional)"), "sk-1");
    await user.click(screen.getByRole("button", { name: "Add" }));
    await screen.findByText("The server is saved. Save again to retry the client secret.");

    await user.type(screen.getByLabelText("Client ID"), "-changed");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(
      await screen.findByText("The server was already added. To change its settings, close this dialog and edit it from the list."),
    ).toBeTruthy();
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  // Stage 2 (M-295): the advanced settings round-trip and are checked.
  it("opens advanced settings that are set and saves them back", async () => {
    const user = userEvent.setup();
    const entry = {
      type: "http",
      url: "https://mcp.example.com/mcp",
      auth: {
        type: "oauth",
        client_id: "pre-1",
        issuer: "https://auth.example.com",
        authorization_endpoint: "https://auth.example.com/authorize?tenant=t",
        token_endpoint: "https://auth.example.com/token",
        resource: "https://api.example.com",
        audience: "my-api",
      },
    };
    const { onSave } = renderDialog({ server: httpServer(entry), hideNameWhenEditing: true, oauthAvailable: true });

    expect(screen.getByLabelText("Authorization endpoint")).toHaveValue("https://auth.example.com/authorize?tenant=t");
    expect(screen.getByLabelText("Token endpoint")).toHaveValue("https://auth.example.com/token");
    expect(screen.getByLabelText("Resource (optional)")).toHaveValue("https://api.example.com");
    expect(screen.getByLabelText("Audience (optional)")).toHaveValue("my-api");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith("gateway", entry));
  });

  it("sets a named issuer and an audience for automatic registration", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ server: httpServer(OAUTH_ENTRY), hideNameWhenEditing: true, oauthAvailable: true });

    expect(screen.queryByLabelText("Issuer (optional)")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.type(await screen.findByLabelText("Issuer (optional)"), "https://auth.example.com");
    await user.type(screen.getByLabelText("Audience (optional)"), "my-api");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(onSave).toHaveBeenCalledWith("gateway", {
        ...OAUTH_ENTRY,
        auth: { type: "oauth", scope: "mcp read", issuer: "https://auth.example.com", audience: "my-api" },
      }),
    );
  });

  it("asks for both manual endpoints and a pre-registered client with them", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ server: httpServer(OAUTH_ENTRY), hideNameWhenEditing: true, oauthAvailable: true });

    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.type(await screen.findByLabelText("Authorization endpoint"), "https://auth.example.com/authorize");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Set both endpoints, or neither.")).toBeTruthy();

    await user.type(screen.getByLabelText("Token endpoint"), "https://auth.example.com/token");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Endpoints set by hand need a pre-registered client.")).toBeTruthy();
    expect(onSave).not.toHaveBeenCalled();
  });

  it("asks for the client ID and an HTTPS issuer", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ oauthAvailable: true, onSaveOAuthClientSecret: vi.fn() });

    await user.type(screen.getByLabelText("Server name"), "gateway");
    await user.click(screen.getByRole("button", { name: /^Streamable HTTP/ }));
    await user.type(screen.getByLabelText("Server URL"), "https://mcp.example.com/mcp");
    await user.click(screen.getByRole("button", { name: "OAuth 2.1" }));
    await user.click(screen.getByRole("button", { name: "Pre-registered" }));
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(await screen.findByText("Enter the client ID.")).toBeTruthy();

    await user.type(screen.getByLabelText("Client ID"), "pre-1");
    await user.type(screen.getByLabelText("Issuer"), "http://auth.example.com");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(await screen.findByText("Enter the issuer as an HTTPS URL.")).toBeTruthy();
    expect(onSave).not.toHaveBeenCalled();
  });

  it("offers no secret field when the page cannot save one, and switches back to automatic registration", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({ server: httpServer(PREREGISTERED_ENTRY), hideNameWhenEditing: true, oauthAvailable: true });
    expect(screen.getByLabelText("Client ID")).toHaveValue("pre-1");
    expect(screen.queryByLabelText("Client secret (optional)")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Register automatically" }));
    expect(screen.queryByLabelText("Client ID")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(onSave).toHaveBeenCalledWith("gateway", { type: "http", url: "https://mcp.example.com/mcp", auth: { type: "oauth" } }),
    );
  });
});
