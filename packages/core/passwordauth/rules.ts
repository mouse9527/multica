// Fork of mouse9527/multica: password login (M-43).
// Mirrors ValidatePassword in server/internal/passwordauth: the server stays
// the authority; this only spares a round trip for the obvious cases.

export const PASSWORD_MIN_CHARS = 12;
export const PASSWORD_MAX_BYTES = 1024;

export type NewPasswordProblem = "too_short" | "too_long" | "mismatch";

/** Counts Unicode code points, as the server does, not UTF-16 units. */
export function passwordCharCount(password: string): number {
  return Array.from(password).length;
}

export function checkNewPassword(
  password: string,
  confirmation: string,
): NewPasswordProblem | null {
  if (passwordCharCount(password) < PASSWORD_MIN_CHARS) return "too_short";
  if (new TextEncoder().encode(password).length > PASSWORD_MAX_BYTES) return "too_long";
  if (password !== confirmation) return "mismatch";
  return null;
}
