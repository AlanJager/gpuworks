import { useCallback, useEffect, useState } from "react";

export interface AnalyticsData<T> {
  data: T | null;
  error: Error | null;
  loading: boolean;
  retry: () => void;
}

// useAnalyticsData runs fetcher whenever deps change and keeps the latest
// answer: an answer to a query that has since been replaced is dropped, and
// the previous answer stays on screen (loading) until the new one arrives so
// a filter change does not blank the page. A failure clears the data.
export function useAnalyticsData<T>(fetcher: (signal: AbortSignal) => Promise<T>, deps: readonly unknown[], enabled = true): AnalyticsData<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [attempt, setAttempt] = useState(0);
  const retry = useCallback(() => setAttempt((n) => n + 1), []);

  useEffect(() => {
    if (!enabled) return;
    const control = new AbortController();
    setLoading(true);
    fetcher(control.signal)
      .then((next) => {
        if (control.signal.aborted) return;
        setData(next);
        setError(null);
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (control.signal.aborted) return;
        setData(null);
        setError(e instanceof Error ? e : new Error(String(e)));
        setLoading(false);
      });
    return () => control.abort();
  }, [...deps, attempt, enabled]);

  return { data, error, loading, retry };
}
