import { useEffect, useRef, useState } from "react";

// usePolling refetches on an interval as an SSE fallback and for freshness.
// Keep the implicit dependency list referentially stable. A default `[]`
// parameter is recreated on every render, which makes the effect below run
// again whenever a request updates state and can turn polling into a tight
// request loop.
const EMPTY_POLLING_DEPS: readonly unknown[] = [];

export function usePolling<T>(
  fetcher: () => Promise<T>,
  intervalMs: number,
  deps?: unknown[],
) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const fRef = useRef(fetcher);
  fRef.current = fetcher;
  const aliveRef = useRef(true);
  const inFlightRef = useRef<Promise<void> | null>(null);
  const trailingRef = useRef(false);
  const load = async (foreground: boolean) => {
    if (foreground) setLoading(true);
    if (inFlightRef.current) {
      trailingRef.current = true;
      if (foreground) {
        return inFlightRef.current.finally(() => {
          if (aliveRef.current) setLoading(false);
        });
      }
      return inFlightRef.current;
    }
    const task = (async () => {
      do {
        trailingRef.current = false;
        try {
          const d = await fRef.current();
          if (aliveRef.current) {
            setData(d);
            setError(null);
            setUpdatedAt(new Date());
          }
        } catch (e) {
          if (aliveRef.current) setError(e as Error);
        }
      } while (trailingRef.current && aliveRef.current);
    })().finally(() => {
      inFlightRef.current = null;
      if (aliveRef.current && foreground) setLoading(false);
    });
    inFlightRef.current = task;
    return task;
  };
  useEffect(() => {
    aliveRef.current = true;
    void load(true);
    const visibleTick = () => {
      if (!document.hidden) void load(false);
    };
    const id =
      intervalMs > 0 ? setInterval(visibleTick, intervalMs) : undefined;
    if (intervalMs > 0)
      document.addEventListener("visibilitychange", visibleTick);
    return () => {
      aliveRef.current = false;
      if (id != null) clearInterval(id);
      if (intervalMs > 0)
        document.removeEventListener("visibilitychange", visibleTick);
    };
  }, deps ?? EMPTY_POLLING_DEPS);
  const refetch = async () => {
    await load(true);
  };
  const refresh = async () => {
    await load(false);
  };
  return { data, error, loading, updatedAt, refetch, refresh };
}
