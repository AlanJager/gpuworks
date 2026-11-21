import { getJSON, sendJSON } from "../http";
import type { AuthUser } from "../auth";

export const authApi = {
  me: () => getJSON<AuthUser>("/api/v1/auth/me"),
  /** Replaces the signed-in user's password and ends all of their sessions. */
  changePassword: (currentPassword: string, newPassword: string) =>
    sendJSON<{ status: string }>("POST", "/api/v1/auth/password", { current_password: currentPassword, new_password: newPassword }),
};
