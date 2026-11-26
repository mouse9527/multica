// @vitest-environment jsdom
// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: RFC 9207 iss of the authorization response (M-295 stage 2).

import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "@multica/core/api";
import { I18nProvider } from "@multica/core/i18n/react";
import enMcpOAuth from "../locales/en/mcpoauth.json";

const mockReplace = vi.hoisted(() => vi.fn());
const mockComplete = vi.hoisted(() => vi.fn());
const searchRef = vi.hoisted(() => ({ current: new URLSearchParams() }));
const authRef = vi.hoisted(() => ({
  current: { user: { id: "user-1" } as { id: string } | null, isLoading: false },
}));

vi.mock("@multica/core/mcp-oauth", () => ({ completeMcpOAuth: mockComplete }));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (selector?: (state: typeof authRef.current) => unknown) =>
      selector ? selector(authRef.current) : authRef.current,
    { getState: () => authRef.current },
  );
  return { useAuthStore };
});

vi.mock("../navigation", () => ({
  useNavigation: () => ({
    replace: mockReplace,
    searchParams: searchRef.current,
    hash: "",
    pathname: "/oauth/mcp",
  }),
}));

import { isAppPath, McpOAuthCallbackPage } from "./mcp-oauth-callback-page";

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={{ en: { mcpoauth: enMcpOAuth } }}>
      {children}
    </I18nProvider>
  );
}

function renderCallback(search: string) {
  searchRef.current = new URLSearchParams(search);
  return render(<McpOAuthCallbackPage />, { wrapper: Wrapper });
}

beforeEach(() => {
  vi.clearAllMocks();
  authRef.current = { user: { id: "user-1" }, isLoading: false };
});

describe("McpOAuthCallbackPage", () => {
  it("completes with the session and returns to the page the connect started from", async () => {
    mockComplete.mockResolvedValue({
      workspace_id: "ws-1",
      agent_id: "agent-1",
      server_id: "s1",
      return_to: "/acme/agents/agent-1?view=mcp_oauth",
    });
    renderCallback("state=st&code=cd");
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/acme/agents/agent-1?view=mcp_oauth"));
    expect(mockComplete).toHaveBeenCalledTimes(1);
    expect(mockComplete).toHaveBeenCalledWith({ state: "st", code: "cd", error: "", iss: "" });
  });

  it("never follows a return path that leaves the app", async () => {
    mockComplete.mockResolvedValue({ workspace_id: "ws-1", agent_id: "", server_id: "", return_to: "//evil.example/x" });
    renderCallback("state=st&code=cd");
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/"));
  });

  it("passes a declined authorization on, so the state is used up", async () => {
    mockComplete.mockRejectedValue(new ApiError("denied", 400, "Bad Request", { code: "mcp_oauth_denied" }));
    renderCallback("state=st&error=access_denied");
    expect(await screen.findByRole("alert")).toHaveTextContent("The authorization was cancelled.");
    expect(mockComplete).toHaveBeenCalledWith({ state: "st", code: "", error: "access_denied", iss: "" });
  });

  // RFC 9207 (M-295 stage 2): iss goes to the server, which checks it; a
  // mismatch shows the generic failure, never the response's error.
  it("passes the response's iss on and does not show the error of a mismatched one", async () => {
    mockComplete.mockRejectedValue(new ApiError("iss", 400, "Bad Request", { code: "mcp_oauth_issuer_response" }));
    renderCallback("state=st&error=access_denied&iss=https%3A%2F%2Fauth.example.com");
    const alert = await screen.findByRole("alert");
    expect(alert).not.toHaveTextContent("The authorization was cancelled.");
    expect(alert).toHaveTextContent("did not complete the connection");
    expect(mockComplete).toHaveBeenCalledWith({ state: "st", code: "", error: "access_denied", iss: "https://auth.example.com" });
  });

  it("explains a used or expired link", async () => {
    mockComplete.mockRejectedValue(new ApiError("bad state", 400, "Bad Request", { code: "mcp_oauth_state_invalid" }));
    renderCallback("state=st&code=cd");
    expect(await screen.findByRole("alert")).toHaveTextContent("already used or has expired");
  });

  it("does not carry the code to the login page when signed out", async () => {
    authRef.current = { user: null, isLoading: false };
    renderCallback("state=st&code=cd");
    expect(await screen.findByRole("alert")).toHaveTextContent("Sign in to Multica in this browser");
    expect(mockComplete).not.toHaveBeenCalled();
    expect(mockReplace).not.toHaveBeenCalled();
  });

  it("needs a state", async () => {
    renderCallback("code=cd");
    expect(await screen.findByRole("alert")).toHaveTextContent("needs the result of an authorization");
    expect(mockComplete).not.toHaveBeenCalled();
  });
});

describe("isAppPath", () => {
  it("accepts only in-app paths", () => {
    expect(isAppPath("/acme/agents/a")).toBe(true);
    expect(isAppPath("//evil.example")).toBe(false);
    expect(isAppPath("/\\evil.example")).toBe(false);
    expect(isAppPath("https://evil.example")).toBe(false);
    expect(isAppPath("")).toBe(false);
    // The browser drops tab and newlines before parsing: "/\t/evil" is "//evil".
    expect(isAppPath("/\t/evil.example")).toBe(false);
    expect(isAppPath("/\n/evil.example")).toBe(false);
    expect(isAppPath("/a\u0000b")).toBe(false);
    expect(isAppPath("/acme/agents/a?view=mcp_oauth")).toBe(true);
  });
});
