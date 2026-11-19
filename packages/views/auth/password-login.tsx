"use client";

// Fork of mouse9527/multica: password login on the sign-in page (M-43).
// login-page.tsx only switches its email step into this mode; the request,
// session setup and error wording live here.

import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { useAuthStore } from "@multica/core/auth";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { api, ApiError, errorCode } from "@multica/core/api";
import { passwordLogin } from "@multica/core/passwordauth";
import { useT } from "../i18n";
import { redirectToCliCallback } from "./login-page";

interface UsePasswordLoginOptions {
  email: string;
  cliCallback?: { url: string; state: string };
  onTokenObtained?: () => void;
  onSuccess: () => void;
  setError: (message: string) => void;
  setLoading: (loading: boolean) => void;
}

export function usePasswordLogin({
  email,
  cliCallback,
  onTokenObtained,
  onSuccess,
  setError,
  setLoading,
}: UsePasswordLoginOptions) {
  const { t } = useT("passwordauth");
  const qc = useQueryClient();
  const [password, setPassword] = useState("");

  const submit = useCallback(
    async (e?: React.FormEvent) => {
      e?.preventDefault();
      if (!email || !password) return;
      setLoading(true);
      setError("");
      try {
        if (cliCallback) {
          // Same as the code login's CLI path: the token goes straight to the
          // CLI's local listener.
          const { token } = await passwordLogin(api, email, password);
          localStorage.setItem("multica_token", token);
          api.setToken(token);
          onTokenObtained?.();
          redirectToCliCallback(cliCallback.url, token, cliCallback.state);
          return;
        }
        await useAuthStore.getState().loginWithPassword(email, password);
        const wsList = await api.listWorkspaces();
        qc.setQueryData(workspaceKeys.list(), wsList);
        onTokenObtained?.();
        onSuccess();
      } catch (err) {
        // The server answers a wrong password, an unknown email and an
        // account without a password identically, and a locked account the
        // same as a rate-limited address; the wording keeps them that way.
        const code = errorCode(err);
        if (code === "invalid_credentials") {
          setError(t(($) => $.login.errors.invalid_credentials));
        } else if (code === "too_many_attempts") {
          setError(t(($) => $.login.errors.too_many_attempts));
        } else if (err instanceof ApiError && err.status === 403) {
          // A temporarily disabled account: same message as the code login.
          setError(err.message);
        } else {
          setError(t(($) => $.login.errors.failed));
        }
        setPassword("");
        setLoading(false);
      }
    },
    [email, password, cliCallback, onTokenObtained, onSuccess, setError, setLoading, qc, t],
  );

  return { password, setPassword, submit };
}

export function PasswordLoginField({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  const { t } = useT("passwordauth");
  return (
    <div className="space-y-2">
      <Label htmlFor="login-password">{t(($) => $.login.password)}</Label>
      <Input
        id="login-password"
        type="password"
        name="password"
        autoComplete="current-password"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        required
      />
    </div>
  );
}
