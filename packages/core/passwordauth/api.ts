// Fork of mouse9527/multica: password login (M-43). Upstream has no such
// endpoints; they exist only when the server sets
// MULTICA_PASSWORD_AUTH_ENABLED (see docs/password-auth.md).
import { z, type ZodType } from "zod";
import type { ApiClient, LoginResponse } from "../api/client";
import { parseWithFallback } from "../api/schema";
import { UserSchema } from "../api/schemas";

// Typed as a plain ZodType, like parseWithFallback's parameter, so the parsed
// value is narrowed to the client's LoginResponse at one place.
const PasswordLoginResponseSchema: ZodType = z.object({
  token: z.string().min(1),
  user: UserSchema,
});

const PasswordStatusSchema = z.object({
  has_password: z.boolean(),
}).loose();

export interface PasswordStatus {
  has_password: boolean;
}

export interface SetPasswordRequest {
  /** Required by the server once the account already has a password. */
  current_password?: string;
  new_password: string;
}

/**
 * POST /auth/password-login. Same response and cookies as the verification
 * code login. A malformed success body throws: there is no safe session to
 * fall back to.
 */
export async function passwordLogin(
  client: ApiClient,
  email: string,
  password: string,
): Promise<LoginResponse> {
  const raw = await client.request("/auth/password-login", {
    method: "POST",
    body: JSON.stringify({ email, password }),
  });
  // safeParse instead of parseWithFallback: its warning logs the received
  // body, and this one carries a session token.
  const parsed = PasswordLoginResponseSchema.safeParse(raw);
  if (!parsed.success) {
    throw new Error("Malformed response from POST /auth/password-login");
  }
  return parsed.data as LoginResponse;
}

export async function getPasswordStatus(client: ApiClient): Promise<PasswordStatus> {
  const raw = await client.request("/api/me/password");
  // Unreadable status falls back to "no password": the form then omits the
  // current-password field, and a server that does need it answers
  // current_password_required, which the form reports.
  return parseWithFallback<PasswordStatus>(raw, PasswordStatusSchema, { has_password: false }, {
    endpoint: "GET /api/me/password",
  });
}

export async function setPassword(
  client: ApiClient,
  data: SetPasswordRequest,
): Promise<void> {
  await client.request("/api/me/password", {
    method: "POST",
    body: JSON.stringify({
      current_password: data.current_password ?? "",
      new_password: data.new_password,
    }),
  });
}
