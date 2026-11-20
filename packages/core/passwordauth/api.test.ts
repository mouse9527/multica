// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import type { ApiClient } from "../api/client";
import { getPasswordStatus, passwordLogin, setPassword } from "./api";
import { passwordAuthConfigStore } from "./config";

// Fake data only; nothing here is a real credential.
const FAKE_PASSWORD = "fake-password-for-tests";

function fakeClient(response: unknown) {
  const request = vi.fn().mockResolvedValue(response);
  return { client: { request } as unknown as ApiClient, request };
}

describe("passwordLogin", () => {
  it("posts the email and password and returns the session", async () => {
    const { client, request } = fakeClient({
      token: "jwt",
      user: { id: "u1", email: "a@example.com", name: "A" },
    });
    const res = await passwordLogin(client, "a@example.com", FAKE_PASSWORD);
    expect(request).toHaveBeenCalledWith("/auth/password-login", {
      method: "POST",
      body: JSON.stringify({ email: "a@example.com", password: FAKE_PASSWORD }),
    });
    expect(res.token).toBe("jwt");
    expect(res.user.id).toBe("u1");
  });

  it("throws on a malformed success body instead of inventing a session", async () => {
    const { client } = fakeClient({ user: { id: "u1" } });
    await expect(passwordLogin(client, "a@example.com", FAKE_PASSWORD)).rejects.toThrow(
      /Malformed response/,
    );
  });

  it("does not put the password in its error", async () => {
    const { client } = fakeClient(null);
    const err = await passwordLogin(client, "a@example.com", FAKE_PASSWORD).catch((e: Error) => e);
    expect(String(err)).not.toContain(FAKE_PASSWORD);
  });
});

describe("getPasswordStatus", () => {
  it("reads has_password", async () => {
    const { client, request } = fakeClient({ has_password: true });
    await expect(getPasswordStatus(client)).resolves.toEqual({ has_password: true });
    expect(request).toHaveBeenCalledWith("/api/me/password");
  });

  it("falls back to no password on a malformed body", async () => {
    const { client } = fakeClient({ has_password: "yes" });
    await expect(getPasswordStatus(client)).resolves.toEqual({ has_password: false });
  });
});

describe("setPassword", () => {
  it("sends an empty current password for a first-time set", async () => {
    const { client, request } = fakeClient({ has_password: true });
    await setPassword(client, { new_password: FAKE_PASSWORD });
    expect(request).toHaveBeenCalledWith("/api/me/password", {
      method: "POST",
      body: JSON.stringify({ current_password: "", new_password: FAKE_PASSWORD }),
    });
  });
});

describe("passwordAuthConfigStore", () => {
  it("only turns on for an explicit true", () => {
    const { setPasswordLoginEnabled } = passwordAuthConfigStore.getState();
    for (const value of [undefined, "true", 1, null, {}]) {
      setPasswordLoginEnabled(value);
      expect(passwordAuthConfigStore.getState().passwordLoginEnabled).toBe(false);
    }
    setPasswordLoginEnabled(true);
    expect(passwordAuthConfigStore.getState().passwordLoginEnabled).toBe(true);
    setPasswordLoginEnabled(undefined);
    expect(passwordAuthConfigStore.getState().passwordLoginEnabled).toBe(false);
  });
});

describe("passwordAuthConfigStore minimum length", () => {
  it("follows a positive integer from the server and falls back to 12 otherwise", () => {
    const { setPasswordMinLength } = passwordAuthConfigStore.getState();
    expect(passwordAuthConfigStore.getState().passwordMinLength).toBe(12);
    setPasswordMinLength(8);
    expect(passwordAuthConfigStore.getState().passwordMinLength).toBe(8);
    for (const value of [undefined, "8", 0, -1, 7.5, null, {}]) {
      setPasswordMinLength(8);
      setPasswordMinLength(value);
      expect(passwordAuthConfigStore.getState().passwordMinLength).toBe(12);
    }
  });
});
