// @vitest-environment jsdom
// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

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
