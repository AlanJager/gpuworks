export interface AuthUser {
  id: number;
  username: string;
  role: string;
  /** The length a new password must have; /auth/me only. */
  password_policy?: { min_length: number; max_length: number };
  /** The effective local administrator floor; /auth/me only, display only. */
  local_admin?: boolean;
}
