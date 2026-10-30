import { useRef } from "react";

/** One key per confirmed preview; retries after a lost response reuse it. */
export function newAgentUpgradeKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export function useAgentUpgradeAction() {
  const active = useRef(false);
  return async (action: () => Promise<void>) => {
    if (active.current) return;
    active.current = true;
    try { await action(); }
    finally { active.current = false; }
  };
}
