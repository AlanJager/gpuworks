import { useEffect, useRef } from "react";
import { BATCH_POLL_MS } from "../../batchApi";

/**
 * useBatchPolling calls refresh every intervalMs while active is true and the
 * tab is visible, and not at all once everything shown has ended: a finished
 * batch never changes again, so an idle page makes no requests. The server
 * checkpoints a running batch's progress about every 5 seconds.
 */
export function useBatchPolling(active: boolean, refresh: () => void, intervalMs = BATCH_POLL_MS) {
  const latest = useRef(refresh);
  latest.current = refresh;
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => {
      if (!document.hidden) latest.current();
    }, intervalMs);
    return () => clearInterval(id);
  }, [active, intervalMs]);
}
