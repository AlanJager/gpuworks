import { useCallback, useEffect, useRef, useState } from "react";

// Monitor queries keep their UI controls mounted, but must never reuse data
// or accept late completions from a different service or time range.
export function useMonitorPolling<T>(fetcher: () => Promise<T>, intervalMs: number, query: string) {
  const [state, setState] = useState<{
    query: string;
    data: T | null;
    error: Error | null;
    loading: boolean;
    updatedAt: Date | null;
  }>({ query, data: null, error: null, loading: true, updatedAt: null });
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const previousIntervalRef = useRef(intervalMs);
  const loadRef = useRef<(foreground: boolean) => Promise<void>>(async () => {});

  useEffect(() => {
    let alive = true;
    let inFlight: Promise<void> | null = null;
    let trailing = false;
    const load = (foreground: boolean): Promise<void> => {
      if (!alive) return Promise.resolve();
      if (foreground) setState((old) => ({ ...old, loading: true }));
      if (inFlight) {
        trailing = true;
        return inFlight;
      }
      inFlight = (async () => {
        do {
          trailing = false;
          try {
            const data = await fetcherRef.current();
            if (alive) setState({ query, data, error: null, loading: false, updatedAt: new Date() });
          } catch (error) {
            if (alive) setState((old) => ({ ...old, error: error as Error, loading: false }));
          }
        } while (trailing && alive);
      })().finally(() => { inFlight = null; });
      return inFlight;
    };
    loadRef.current = load;
    setState((old) => old.query === query ? old : { query, data: null, error: null, loading: true, updatedAt: null });
    void load(true);
    return () => { alive = false; };
  }, [query]);

  useEffect(() => {
    const tick = () => {
      if (!document.hidden) void loadRef.current(false);
    };
    if (intervalMs > 0 && previousIntervalRef.current !== intervalMs) void loadRef.current(false);
    previousIntervalRef.current = intervalMs;
    const timer = intervalMs > 0 ? setInterval(tick, intervalMs) : undefined;
    if (intervalMs > 0) document.addEventListener("visibilitychange", tick);
    return () => {
      if (timer !== undefined) clearInterval(timer);
      if (intervalMs > 0) document.removeEventListener("visibilitychange", tick);
    };
  }, [intervalMs]);

  const refetch = useCallback(() => loadRef.current(true), []);
  const refresh = useCallback(() => loadRef.current(false), []);
  // Effects run after rendering: hide the previous query immediately.
  const current = state.query === query ? state : { data: null, error: null, loading: true, updatedAt: null };
  return { ...current, refetch, refresh };
}
