import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement, ReactNode } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import { passwordAuthConfigStore } from "@multica/core/passwordauth";
import enCommon from "../locales/en/common.json";
import enAuth from "../locales/en/auth.json";
import enPasswordauth from "../locales/en/passwordauth.json";

// Fake data only; nothing here is a real credential.
const FAKE_PASSWORD = "fake-password-for-tests";

const TEST_RESOURCES = {
  en: { common: enCommon, auth: enAuth, passwordauth: enPasswordauth },
};

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function renderWithI18n(ui: ReactElement) {
  return render(ui, { wrapper: I18nWrapper });
}

const mockSendCode = vi.hoisted(() => vi.fn());
const mockLoginWithPassword = vi.hoisted(() => vi.fn());
const mockApiListWorkspaces = vi.hoisted(() => vi.fn());
const mockApiRequest = vi.hoisted(() => vi.fn());
const mockApiSetToken = vi.hoisted(() => vi.fn());
const mockApiGetMe = vi.hoisted(() => vi.fn());
const mockSetQueryData = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", async () => {
  const actual = await vi.importActual<typeof import("@tanstack/react-query")>(
    "@tanstack/react-query",
  );
  return { ...actual, useQueryClient: () => ({ setQueryData: mockSetQueryData }) };
});

vi.mock("@multica/core/auth", () => {
  const state = {
    sendCode: mockSendCode,
    loginWithPassword: mockLoginWithPassword,
    expired: false,
  };
  return {
    useAuthStore: Object.assign(
      (selector?: (s: unknown) => unknown) => (selector ? selector(state) : state),
      { getState: () => state },
    ),
  };
});

vi.mock("@multica/core/api", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/api")>("@multica/core/api");
  return {
    ...actual,
    api: {
      listWorkspaces: mockApiListWorkspaces,
      request: mockApiRequest,
      setToken: mockApiSetToken,
      getMe: mockApiGetMe,
    },
  };
});

import { LoginPage } from "./login-page";

function apiError(status: number, code: string) {
  return new ApiError("server text", status, "", { error: "server text", code });
}

describe("LoginPage password mode", () => {
  const onSuccess = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    mockApiGetMe.mockRejectedValue(new Error("unauthorized"));
    passwordAuthConfigStore.getState().setPasswordLoginEnabled(true);
    Object.defineProperty(window, "location", {
      writable: true,
      value: { href: "http://localhost:3000" },
    });
  });

  afterEach(() => {
    passwordAuthConfigStore.getState().setPasswordLoginEnabled(false);
  });

  it("matches upstream when the server switch is off", () => {
    passwordAuthConfigStore.getState().setPasswordLoginEnabled(false);
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with password" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeInTheDocument();
    expect(screen.getByLabelText(/email/i)).not.toHaveAttribute("autocomplete");
  });

  it("offers email and password fields that password managers recognise", () => {
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    expect(screen.getByLabelText(/email/i)).toHaveAttribute("type", "email");
    expect(screen.getByLabelText(/email/i)).toHaveAttribute("autocomplete", "username");
    const password = screen.getByLabelText("Password");
    expect(password).toHaveAttribute("type", "password");
    expect(password).toHaveAttribute("autocomplete", "current-password");
    expect(screen.getByRole("button", { name: "Sign in" })).toBeDisabled();
  });

  it("signs in, seeds the workspace list, then calls onSuccess", async () => {
    mockLoginWithPassword.mockResolvedValueOnce({ id: "u1" });
    mockApiListWorkspaces.mockResolvedValueOnce([{ id: "ws-1" }]);
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/email/i), "a@example.com");
    await user.type(screen.getByLabelText("Password"), FAKE_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    expect(mockLoginWithPassword).toHaveBeenCalledWith("a@example.com", FAKE_PASSWORD);
    expect(mockSetQueryData).toHaveBeenCalledWith(
      expect.arrayContaining(["workspaces", "list"]),
      [{ id: "ws-1" }],
    );
    expect(mockSendCode).not.toHaveBeenCalled();
  });

  it("says only that the email or password is wrong, and clears the password", async () => {
    mockLoginWithPassword.mockRejectedValueOnce(apiError(401, "invalid_credentials"));
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/email/i), "a@example.com");
    await user.type(screen.getByLabelText("Password"), FAKE_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByText("Incorrect email or password.")).toBeInTheDocument();
    expect(screen.queryByText("server text")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(onSuccess).not.toHaveBeenCalled();
  });

  it("gives one message for a locked account and a rate limit", async () => {
    mockLoginWithPassword.mockRejectedValueOnce(apiError(429, "too_many_attempts"));
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/email/i), "a@example.com");
    await user.type(screen.getByLabelText("Password"), FAKE_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    expect(
      await screen.findByText("Too many attempts. Wait a few minutes and try again."),
    ).toBeInTheDocument();
  });

  it("keeps the email code login as a fallback", async () => {
    mockSendCode.mockResolvedValueOnce(undefined);
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Email me a code instead" }));
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();

    await user.type(screen.getByLabelText(/email/i), "a@example.com");
    await user.click(screen.getByRole("button", { name: "Continue" }));

    await waitFor(() => expect(mockSendCode).toHaveBeenCalledWith("a@example.com"));
    expect(mockLoginWithPassword).not.toHaveBeenCalled();
  });

  it("can switch back to the password form", async () => {
    renderWithI18n(<LoginPage onSuccess={onSuccess} />);

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Email me a code instead" }));
    await user.click(screen.getByRole("button", { name: "Sign in with password" }));
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
  });

  it("keeps the Google login next to the password form", () => {
    renderWithI18n(
      <LoginPage
        onSuccess={onSuccess}
        google={{ clientId: "id", redirectUri: "http://localhost:3000/auth/callback" }}
      />,
    );
    expect(screen.getByRole("button", { name: "Continue with Google" })).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
  });

  it("hands the token to the CLI callback", async () => {
    mockApiRequest.mockResolvedValueOnce({ token: "cli-jwt", user: { id: "u1" } });
    renderWithI18n(
      <LoginPage
        onSuccess={onSuccess}
        cliCallback={{ url: "http://localhost:9876/callback", state: "s1" }}
      />,
    );

    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/email/i), "a@example.com");
    await user.type(screen.getByLabelText("Password"), FAKE_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() =>
      expect(window.location.href).toBe(
        "http://localhost:9876/callback?token=cli-jwt&state=s1",
      ),
    );
    expect(mockApiRequest).toHaveBeenCalledWith("/auth/password-login", {
      method: "POST",
      body: JSON.stringify({ email: "a@example.com", password: FAKE_PASSWORD }),
    });
    expect(mockLoginWithPassword).not.toHaveBeenCalled();
  });
});
