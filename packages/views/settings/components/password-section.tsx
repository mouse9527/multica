"use client";

// Fork of mouse9527/multica: set or change the account password (M-43).
// Rendered from the profile tab; hidden unless the server enables password
// login, so the tab matches upstream while the switch is off.

import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { useAuthStore } from "@multica/core/auth";
import { errorCode } from "@multica/core/api";
import {
  checkNewPassword,
  passwordCharCount,
  passwordStatusOptions,
  PASSWORD_MIN_CHARS,
  usePasswordLoginEnabled,
  useSetPassword,
  type NewPasswordProblem,
} from "@multica/core/passwordauth";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection } from "./settings-layout";

export function PasswordSettingsSection() {
  const enabled = usePasswordLoginEnabled();
  if (!enabled) return null;
  return <PasswordSettingsForm />;
}

function PasswordSettingsForm() {
  const { t } = useT("passwordauth");
  const email = useAuthStore((s) => s.user?.email ?? "");
  const status = useQuery(passwordStatusOptions());
  const setPassword = useSetPassword();
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState("");

  const hasPassword = status.data?.has_password === true;

  const problemMessage = (problem: NewPasswordProblem) => {
    switch (problem) {
      case "too_short":
        return t(($) => $.settings.errors.too_short);
      case "too_long":
        return t(($) => $.settings.errors.too_long);
      case "mismatch":
        return t(($) => $.settings.errors.mismatch);
    }
  };

  const serverMessage = (err: unknown) => {
    switch (errorCode(err)) {
      case "invalid_password":
        return passwordCharCount(newPassword) < PASSWORD_MIN_CHARS
          ? t(($) => $.settings.errors.too_short)
          : t(($) => $.settings.errors.too_long);
      case "current_password_required":
        return t(($) => $.settings.errors.current_password_required);
      case "current_password_incorrect":
        return t(($) => $.settings.errors.current_password_incorrect);
      case "too_many_attempts":
        return t(($) => $.settings.errors.too_many_attempts);
      case "password_reset_required":
        return t(($) => $.settings.errors.password_reset_required);
      default:
        return t(($) => $.settings.errors.failed);
    }
  };

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (hasPassword && !currentPassword) {
      setError(t(($) => $.settings.errors.current_password_required));
      return;
    }
    const problem = checkNewPassword(newPassword, confirmPassword);
    if (problem) {
      setError(problemMessage(problem));
      return;
    }
    setError("");
    const changing = hasPassword;
    setPassword
      .mutateAsync({
        current_password: changing ? currentPassword : undefined,
        new_password: newPassword,
      })
      .then(
        () => {
          setCurrentPassword("");
          setNewPassword("");
          setConfirmPassword("");
          toast.success(
            changing
              ? t(($) => $.settings.toast_changed)
              : t(($) => $.settings.toast_set),
          );
        },
        (err: unknown) => {
          if (errorCode(err) === "current_password_incorrect") setCurrentPassword("");
          setError(serverMessage(err));
        },
      );
  };

  if (status.isError) {
    return (
      <SettingsSection title={t(($) => $.settings.title)} anchor="password">
        <p className="text-body text-destructive">
          {t(($) => $.settings.errors.load_failed)}
        </p>
      </SettingsSection>
    );
  }

  if (status.isPending) return null;

  const busy = setPassword.isPending;

  return (
    <form onSubmit={handleSubmit} noValidate>
      <SettingsSection
        title={t(($) => $.settings.title)}
        description={hasPassword ? undefined : t(($) => $.settings.set_description)}
        anchor="password"
      >
        {/* Lets password managers tie the saved password to this account. */}
        <input
          type="email"
          name="username"
          autoComplete="username"
          value={email}
          readOnly
          hidden
        />
        <SettingsCard>
          {hasPassword && (
            <SettingsRow label={t(($) => $.settings.current_password)} size="text">
              <Input
                type="password"
                name="current-password"
                autoComplete="current-password"
                aria-label={t(($) => $.settings.current_password)}
                value={currentPassword}
                onChange={(event) => setCurrentPassword(event.target.value)}
                disabled={busy}
              />
            </SettingsRow>
          )}
          <SettingsRow
            label={t(($) => $.settings.new_password)}
            description={t(($) => $.settings.new_password_hint)}
            size="text"
          >
            <Input
              type="password"
              name="new-password"
              autoComplete="new-password"
              aria-label={t(($) => $.settings.new_password)}
              value={newPassword}
              onChange={(event) => setNewPassword(event.target.value)}
              disabled={busy}
            />
          </SettingsRow>
          <SettingsRow label={t(($) => $.settings.confirm_password)} size="text">
            <Input
              type="password"
              name="confirm-password"
              autoComplete="new-password"
              aria-label={t(($) => $.settings.confirm_password)}
              value={confirmPassword}
              onChange={(event) => setConfirmPassword(event.target.value)}
              disabled={busy}
            />
          </SettingsRow>
        </SettingsCard>
        <div className="flex flex-col items-stretch gap-2 sm:flex-row sm:items-center sm:justify-end">
          {error && (
            <p role="alert" className="text-body text-destructive sm:mr-auto">
              {error}
            </p>
          )}
          <Button type="submit" disabled={busy || !newPassword || !confirmPassword}>
            {setPassword.isPending
              ? t(($) => $.settings.saving)
              : hasPassword
                ? t(($) => $.settings.submit_change)
                : t(($) => $.settings.submit_set)}
          </Button>
        </div>
      </SettingsSection>
    </form>
  );
}
