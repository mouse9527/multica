// Fork of mouse9527/multica: password login (M-43).
export { passwordLogin, getPasswordStatus, setPassword } from "./api";
export type { PasswordStatus, SetPasswordRequest } from "./api";
export { passwordAuthConfigStore, usePasswordLoginEnabled } from "./config";
export {
  checkNewPassword,
  passwordCharCount,
  PASSWORD_MAX_BYTES,
  PASSWORD_MIN_CHARS,
} from "./rules";
export type { NewPasswordProblem } from "./rules";
export { passwordAuthKeys, passwordStatusOptions, useSetPassword } from "./queries";
