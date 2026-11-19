// Fork of mouse9527/multica: password login (M-43).
import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { getPasswordStatus, setPassword, type PasswordStatus, type SetPasswordRequest } from "./api";

// Account-scoped: a password belongs to the user, not a workspace.
export const passwordAuthKeys = {
  status: () => ["me", "password"] as const,
};

export function passwordStatusOptions() {
  return queryOptions({
    queryKey: passwordAuthKeys.status(),
    queryFn: () => getPasswordStatus(api),
  });
}

export function useSetPassword() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: SetPasswordRequest) => setPassword(api, data),
    onSuccess: () => {
      qc.setQueryData<PasswordStatus>(passwordAuthKeys.status(), { has_password: true });
    },
  });
}
