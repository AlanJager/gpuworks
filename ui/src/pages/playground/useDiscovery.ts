import { useCallback, useEffect, useState } from "react";
import { apiResponseError } from "../../apiError";
import type { DiscoveryRoute } from "../../inferenceApi";
import type { PlaygroundModel } from "./types";

const cache = new Map<string, { expires: number; data: unknown }>();
const CACHE_MS = 30_000;
interface DiscoveryState<T> { key: string; data?: T; error?: string; loading: boolean }

// Failure never blocks the form; only successful, deployment-scoped facts are cached.
export function useDiscovery<T = unknown>(model: PlaygroundModel | undefined, route: DiscoveryRoute | undefined) {
  const id = model?.id;
  const endpoint = route?.endpoint;
  const enabled = Boolean(id && endpoint && !model?.external && !model?.sleeping);
  const key = JSON.stringify([id, model?.deployment_id, endpoint]);
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<DiscoveryState<T>>({ key: "", loading: false });
  const reload = useCallback(() => { cache.delete(key); setRevision((value) => value + 1); }, [key]);

  useEffect(() => {
    if (!enabled) { cache.delete(key); setState({ key, loading: false }); return; }
    const cached = cache.get(key);
    if (cached && cached.expires > Date.now()) { setState({ key, loading: false, data: cached.data as T }); return; }
    const controller = new AbortController();
    setState({ key, loading: true });
    void (async () => {
      try {
        const response = await fetch(`/api/v1/playground${endpoint}?${new URLSearchParams({ model: id! })}`, { credentials: "include", signal: controller.signal });
        if (!response.ok) {
          const status = `${response.status} ${response.statusText}`.trim();
          const failure = await apiResponseError(response, status);
          throw new Error(`${status}: ${failure.message}`);
        }
        const data = await response.json() as T;
        if (controller.signal.aborted) return;
        // Bound memory even when operators browse many models in one session.
        if (cache.size >= 128) cache.delete(cache.keys().next().value!);
        cache.set(key, { data, expires: Date.now() + CACHE_MS });
        setState({ key, loading: false, data });
      } catch (error) {
        if (!controller.signal.aborted) setState({ key, loading: false, error: (error as Error).message });
      }
    })();
    return () => controller.abort();
  }, [key, id, endpoint, enabled, revision]);

  const current = enabled && state.key === key ? state : { loading: enabled };
  return { data: "data" in current ? current.data : undefined, error: "error" in current ? current.error : undefined, loading: current.loading, reload };
}
