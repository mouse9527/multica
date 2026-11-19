import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import { passwordAuthConfigStore } from "@multica/core/passwordauth";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";
import enPasswordauth from "../../locales/en/passwordauth.json";

// Fake data only; nothing here is a real credential.
const OLD_PASSWORD = "fake-old-password";
const NEW_PASSWORD = "fake-new-password-123";

const mockRequest = vi.hoisted(() => vi.fn());
const mockToastSuccess = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/api")>("@multica/core/api");
  return { ...actual, api: { request: mockRequest } };
});

vi.mock("@multica/core/auth", () => {
  const state = { user: { id: "u1", email: "a@example.com" } };
  return {
    useAuthStore: Object.assign(
      (selector?: (s: unknown) => unknown) => (selector ? selector(state) : state),
      { getState: () => state },
    ),
  };
});

vi.mock("sonner", () => ({ toast: { success: mockToastSuccess, error: vi.fn() } }));

import { PasswordSettingsSection } from "./password-section";

const TEST_RESOURCES = {
  en: { common: enCommon, settings: enSettings, passwordauth: enPasswordauth },
};

function renderSection() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={qc}>
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          {children}
        </I18nProvider>
      </QueryClientProvider>
    );
  }
  return render(<PasswordSettingsSection />, { wrapper: Wrapper });
}

// Routes the fake API: GET status, POST set.
function serve(hasPassword: boolean, onSet?: () => Promise<unknown>) {
  mockRequest.mockImplementation((path: string, init?: RequestInit) => {
    if (path === "/api/me/password" && !init?.method) {
      return Promise.resolve({ has_password: hasPassword });
    }
    if (path === "/api/me/password" && init?.method === "POST") {
      return onSet ? onSet() : Promise.resolve({ has_password: true });
    }
    return Promise.reject(new Error(`unexpected ${path}`));
  });
}

function postedBodies() {
  return mockRequest.mock.calls
    .filter(([, init]) => (init as RequestInit | undefined)?.method === "POST")
    .map(([, init]) => JSON.parse((init as RequestInit).body as string));
}

describe("PasswordSettingsSection", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    passwordAuthConfigStore.getState().setPasswordLoginEnabled(true);
  });

  afterEach(() => {
    passwordAuthConfigStore.getState().setPasswordLoginEnabled(false);
  });

  it("renders nothing and asks nothing while the server switch is off", () => {
    passwordAuthConfigStore.getState().setPasswordLoginEnabled(false);
    const { container } = renderSection();
    expect(container).toBeEmptyDOMElement();
    expect(mockRequest).not.toHaveBeenCalled();
  });

  it("sets a first password without asking for a current one", async () => {
    serve(false);
    renderSection();

    const user = userEvent.setup();
    const newField = await screen.findByLabelText("New password");
    expect(screen.queryByLabelText("Current password")).not.toBeInTheDocument();
    expect(newField).toHaveAttribute("autocomplete", "new-password");
    expect(screen.getByLabelText("Confirm new password")).toHaveAttribute(
      "autocomplete",
      "new-password",
    );

    await user.type(newField, NEW_PASSWORD);
    await user.type(screen.getByLabelText("Confirm new password"), NEW_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Set password" }));

    await waitFor(() => expect(mockToastSuccess).toHaveBeenCalledWith("Password set"));
    expect(postedBodies()).toEqual([{ current_password: "", new_password: NEW_PASSWORD }]);
    // The cache now says the account has a password, so the form asks for it.
    expect(await screen.findByLabelText("Current password")).toBeInTheDocument();
  });

  it("changes a password with the current one", async () => {
    serve(true);
    renderSection();

    const user = userEvent.setup();
    const current = await screen.findByLabelText("Current password");
    expect(current).toHaveAttribute("autocomplete", "current-password");
    await user.type(current, OLD_PASSWORD);
    await user.type(screen.getByLabelText("New password"), NEW_PASSWORD);
    await user.type(screen.getByLabelText("Confirm new password"), NEW_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Change password" }));

    await waitFor(() => expect(mockToastSuccess).toHaveBeenCalledWith("Password changed"));
    expect(postedBodies()).toEqual([
      { current_password: OLD_PASSWORD, new_password: NEW_PASSWORD },
    ]);
    expect(screen.getByLabelText("New password")).toHaveValue("");
  });

  it("checks length and confirmation before calling the server", async () => {
    serve(false);
    renderSection();

    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("New password"), "short");
    await user.type(screen.getByLabelText("Confirm new password"), "short");
    await user.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("Use at least 12 characters.")).toBeInTheDocument();

    await user.clear(screen.getByLabelText("New password"));
    await user.type(screen.getByLabelText("New password"), NEW_PASSWORD);
    await user.clear(screen.getByLabelText("Confirm new password"));
    await user.type(screen.getByLabelText("Confirm new password"), `${NEW_PASSWORD}x`);
    await user.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("The new passwords don't match.")).toBeInTheDocument();

    expect(postedBodies()).toEqual([]);
  });

  it("reports a wrong current password and clears it", async () => {
    serve(true, () =>
      Promise.reject(
        new ApiError("server text", 403, "", { code: "current_password_incorrect" }),
      ),
    );
    renderSection();

    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Current password"), OLD_PASSWORD);
    await user.type(screen.getByLabelText("New password"), NEW_PASSWORD);
    await user.type(screen.getByLabelText("Confirm new password"), NEW_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Change password" }));

    expect(await screen.findByText("Current password is incorrect.")).toBeInTheDocument();
    expect(screen.getByLabelText("Current password")).toHaveValue("");
    expect(mockToastSuccess).not.toHaveBeenCalled();
  });

  it("points to an administrator when the stored password cannot be checked", async () => {
    serve(true, () =>
      Promise.reject(
        new ApiError("server text", 409, "", { code: "password_reset_required" }),
      ),
    );
    renderSection();

    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Current password"), OLD_PASSWORD);
    await user.type(screen.getByLabelText("New password"), NEW_PASSWORD);
    await user.type(screen.getByLabelText("Confirm new password"), NEW_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Change password" }));

    expect(
      await screen.findByText(
        "Your current password can't be checked right now. Ask an administrator to reset it.",
      ),
    ).toBeInTheDocument();
  });

  it("asks for the current password before sending a change", async () => {
    serve(true);
    renderSection();

    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("New password"), NEW_PASSWORD);
    await user.type(screen.getByLabelText("Confirm new password"), NEW_PASSWORD);
    await user.click(screen.getByRole("button", { name: "Change password" }));

    expect(await screen.findByText("Enter your current password.")).toBeInTheDocument();
    expect(postedBodies()).toEqual([]);
  });
});
