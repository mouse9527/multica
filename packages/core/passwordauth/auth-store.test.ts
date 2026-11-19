import { describe, expect, it, vi } from "vitest";
import type { ApiClient } from "../api/client";
import type { StorageAdapter } from "../types";
import { createAuthStore } from "../auth/store";

// Fake data only; nothing here is a real credential.
const FAKE_PASSWORD = "fake-password-for-tests";
const fakeUser = { id: "u1", name: "Alice", email: "alice@example.com" };

function makeStorage(): StorageAdapter & { data: Record<string, string> } {
  const data: Record<string, string> = {};
  return {
    data,
    getItem: (k) => data[k] ?? null,
    setItem: (k, v) => {
      data[k] = v;
    },
    removeItem: (k) => {
      delete data[k];
    },
  };
}

function makeApi() {
  return {
    setToken: vi.fn(),
    request: vi.fn().mockResolvedValue({ token: "jwt", user: fakeUser }),
  };
}

describe("authStore.loginWithPassword", () => {
  it("in token mode stores the token like the code login", async () => {
    const storage = makeStorage();
    const api = makeApi();
    const onLogin = vi.fn();
    const store = createAuthStore({ api: api as unknown as ApiClient, storage, onLogin });

    const user = await store.getState().loginWithPassword("alice@example.com", FAKE_PASSWORD);

    expect(user.id).toBe("u1");
    expect(api.request).toHaveBeenCalledWith("/auth/password-login", expect.anything());
    expect(storage.data.multica_token).toBe("jwt");
    expect(api.setToken).toHaveBeenCalledWith("jwt");
    expect(onLogin).toHaveBeenCalled();
    expect(store.getState().status).toBe("authenticated");
  });

  it("in cookie mode leaves the token to the server's cookie", async () => {
    const storage = makeStorage();
    const api = makeApi();
    const store = createAuthStore({ api: api as unknown as ApiClient, storage, cookieAuth: true });

    await store.getState().loginWithPassword("alice@example.com", FAKE_PASSWORD);

    expect(storage.data.multica_token).toBeUndefined();
    expect(api.setToken).not.toHaveBeenCalled();
    expect(store.getState().user?.id).toBe("u1");
  });

  it("stays signed out when the server refuses", async () => {
    const storage = makeStorage();
    const api = makeApi();
    api.request.mockRejectedValueOnce(new Error("invalid email or password"));
    const store = createAuthStore({ api: api as unknown as ApiClient, storage });
    store.setState({ status: "unauthenticated", isLoading: false });

    await expect(
      store.getState().loginWithPassword("alice@example.com", FAKE_PASSWORD),
    ).rejects.toThrow();
    expect(store.getState().status).toBe("unauthenticated");
    expect(storage.data.multica_token).toBeUndefined();
  });
});
