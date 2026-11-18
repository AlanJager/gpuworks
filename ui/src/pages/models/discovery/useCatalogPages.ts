import { useCallback, useEffect, useRef, useState } from "react";
import { catalogSignal, catalogUrl, pageSize, problem, readPage, type HubModel, type Provider } from "./hubs";
import type { OperationType } from "../../../inferenceApi";

interface CatalogResult {
  items: HubModel[];
  total?: number;
  next?: string;
  pages: number;
}
interface CatalogState {
  key: string;
  result?: CatalogResult;
  loading: boolean;
  error: string;
  failedMore: boolean;
}

export function useCatalogPages(active: boolean, source: Provider, query: string, capability: OperationType | "all", sort: string) {
  const firstURL = catalogUrl(source, query, capability, sort, 1);
  const cache = useRef(new Map<string, CatalogResult>());
  const controller = useRef<AbortController | null>(null);
  const busy = useRef(false);
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<CatalogState>({key: firstURL, loading: true, error: "", failedMore: false});
  const result = state.key === firstURL ? state.result : cache.current.get(firstURL);
  const error = state.key === firstURL ? state.error : "";
  const loading = state.key !== firstURL || state.loading;

  const load = useCallback(async (more: boolean, current: AbortController) => {
    busy.current = true;
    const previous = cache.current.get(firstURL);
    setState({key: firstURL, result: previous, loading: true, error: "", failedMore: false});
    try {
      let items = more ? [...previous!.items] : [];
      let page = more ? previous!.pages : 0;
      let next = more ? previous!.next : firstURL;
      let total = previous?.total;
      // Refresh the cursor chain together so cached pages never mix old and new cursors.
      const lastPage = more ? page + 1 : Math.max(1, previous?.pages ?? 1);
      const visited = new Set<string>();
      while (next && page < lastPage && !visited.has(next)) {
        visited.add(next);
        const data = await readPage(source, next, catalogSignal(current.signal, 6000));
        current.signal.throwIfAborted();
        items = [...new Map([...items, ...data.items].map(item => [item.id, item])).values()];
        page++;
        total = data.total;
        next = source === "modelscope"
          ? page * pageSize < Math.min(total ?? 0, 3000) ? catalogUrl(source, query, capability, sort, page + 1) : undefined
          : data.next;
        if (!data.items.length || (next && visited.has(next))) next = undefined;
      }
      const updated = {items, total, next, pages: page};
      cache.current.delete(firstURL);
      cache.current.set(firstURL, updated);
      if (cache.current.size > 8) cache.current.delete(cache.current.keys().next().value!);
      setState({key: firstURL, result: updated, loading: false, error: "", failedMore: false});
    } catch (cause) {
      if (current.signal.aborted) return;
      setState({key: firstURL, result: previous, loading: false, error: problem(cause), failedMore: more});
    } finally {
      if (controller.current === current) busy.current = false;
    }
  }, [firstURL, source, query, capability, sort]);

  useEffect(() => {
    if (!active) return;
    const current = new AbortController();
    controller.current = current;
    busy.current = true;
    setState({key: firstURL, result: cache.current.get(firstURL), loading: true, error: "", failedMore: false});
    const timer = setTimeout(() => { void load(false, current); }, 220);
    return () => {
      clearTimeout(timer);
      current.abort();
    };
  }, [active, firstURL, load, attempt]);

  const loadMore = useCallback(() => {
    const current = controller.current;
    if (!active || !current || current.signal.aborted || busy.current || !cache.current.get(firstURL)?.next) return;
    void load(true, current);
  }, [active, firstURL, load]);
  const retry = () => {
    if (state.failedMore) loadMore();
    else setAttempt(value => value + 1);
  };
  return {result, error, loading, loadMore, retry};
}
