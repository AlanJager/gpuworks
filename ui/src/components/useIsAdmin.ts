import { useEffect, useState } from "react";
import { api } from "../api";

/** Whether the signed-in user is an admin; false until known and when the lookup fails. */
export function useIsAdmin(): boolean {
  const [isAdmin, setIsAdmin] = useState(false);
  useEffect(() => {
    let alive = true;
    api.me().then((user) => { if (alive) setIsAdmin(user.role === "admin"); }).catch(() => {});
    return () => { alive = false; };
  }, []);
  return isAdmin;
}
