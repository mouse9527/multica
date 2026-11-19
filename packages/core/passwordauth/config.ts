// Fork of mouse9527/multica: password login (M-43).
import { createStore } from "zustand/vanilla";
import { useStore } from "zustand";

interface PasswordAuthConfigState {
  // Mirrors `password_login_enabled` from /api/config. The server omits the
  // field while the switch is off, and upstream servers never send it, so
  // anything but an explicit `true` keeps every password affordance hidden.
  passwordLoginEnabled: boolean;
  setPasswordLoginEnabled: (enabled?: unknown) => void;
}

export const passwordAuthConfigStore = createStore<PasswordAuthConfigState>((set) => ({
  passwordLoginEnabled: false,
  setPasswordLoginEnabled: (enabled) => set({ passwordLoginEnabled: enabled === true }),
}));

export function usePasswordLoginEnabled(): boolean {
  return useStore(passwordAuthConfigStore, (state) => state.passwordLoginEnabled);
}
