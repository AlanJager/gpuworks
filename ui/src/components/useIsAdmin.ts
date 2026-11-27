import { useEffect, useState } from "react";
import { api } from "../api";
import { useSessionUser } from "./sessionUser";

/**
 * Whether the signed-in user is an admin; false until known and when the lookup fails.
 * The user the session boundary has read answers; /auth/me is asked only while it has none
 * (outside the boundary, or just after a sign-in).
 */
export function useIsAdmin(enabled = true): boolean {
  const sessionUser = useSessionUser();
  const [isAdmin, setIsAdmin] = useState(false);
  const ask = enabled && !sessionUser;
  useEffect(() => {
    if (!ask) return;
    let alive = true;
    api.me().then((user) => { if (alive) setIsAdmin(user.role === "admin"); }).catch(() => {});
    return () => { alive = false; };
  }, [ask]);
  return enabled && (sessionUser ? sessionUser.role === "admin" : isAdmin);
}
