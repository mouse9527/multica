// Fork of mouse9527/multica: password login (M-43, M-184).
import { createStore } from "zustand/vanilla";
import { useStore } from "zustand";
import { PASSWORD_MIN_CHARS } from "./rules";

interface PasswordAuthConfigState {
  // Mirrors `password_login_enabled` from /api/config. The server omits the
  // field while the switch is off, and upstream servers never send it, so
  // anything but an explicit `true` keeps every password affordance hidden.
  passwordLoginEnabled: boolean;
  setPasswordLoginEnabled: (enabled?: unknown) => void;
  // Mirrors `password_min_length` from /api/config. Servers that predate the
  // setting omit it and require PASSWORD_MIN_CHARS.
  passwordMinLength: number;
  setPasswordMinLength: (length?: unknown) => void;
}

export const passwordAuthConfigStore = createStore<PasswordAuthConfigState>((set) => ({
  passwordLoginEnabled: false,
  setPasswordLoginEnabled: (enabled) => set({ passwordLoginEnabled: enabled === true }),
  passwordMinLength: PASSWORD_MIN_CHARS,
  setPasswordMinLength: (length) =>
    set({
      passwordMinLength:
        typeof length === "number" && Number.isInteger(length) && length > 0
          ? length
          : PASSWORD_MIN_CHARS,
    }),
}));

export function usePasswordLoginEnabled(): boolean {
  return useStore(passwordAuthConfigStore, (state) => state.passwordLoginEnabled);
}

export function usePasswordMinLength(): number {
  return useStore(passwordAuthConfigStore, (state) => state.passwordMinLength);
}
