import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { Input } from "../../components/ui/input";
import type { NetworkAttachment, NetworkNode, NetworkNodeFilters } from "../../networkTypes";

export type NetworkSearchTarget = NetworkAttachment | NetworkNode;
export type NetworkSearchKind = "nodes" | "attachments";
type Match = { ids: Set<string>; expires: number };
type Answer = { cache: Map<string, Match>; key: string; attempt: number; ids?: Set<string>; error?: Error };

/** Match across the full network before paginating or selecting rows. */
export function useNetworkTargetSearch<T extends NetworkSearchTarget>(networkId: number, kind: NetworkSearchKind, data: T[], label: string, filters?: NetworkNodeFilters, overrides?: NetworkAttachment[]) {
  const { t: common } = useTranslation("common");
  const [input, setInput] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [answer, setAnswer] = useState<Answer | null>(null);
  const query = input.trim();
  const roles = filters?.roles ?? [];
  const sources = filters?.source ?? [];
  const hasCriteria = query !== "" || roles.length > 0 || sources.length > 0;
  const key = hasCriteria ? JSON.stringify([query, roles, sources]) : "";
  // A detail refresh replaces its arrays even when the searchable data is
  // identical. Keep matches across those refreshes, including an in-flight
  // request. Direct overrides belong to the node search even if its effective
  // endpoint stayed the same.
  const revision = useMemo(() => hasCriteria ? JSON.stringify([data, overrides]) : "", [hasCriteria, data, overrides]);
  const recent = useMemo(() => new Map<string, Match>(), [revision, networkId, kind]);

  useEffect(() => {
    if (!key) return;
    const cached = recent.get(key);
    if (cached && cached.expires > Date.now()) {
      setAnswer({ cache: recent, key, attempt, ids: cached.ids });
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(async () => {
      try {
        const result = await api.searchNetworkTargets(networkId, kind, query, controller.signal, filters);
        if (controller.signal.aborted) return;
        const ids = new Set(result.ids);
        for (const [cachedKey, entry] of recent) if (entry.expires <= Date.now()) recent.delete(cachedKey);
        recent.set(key, { ids, expires: Date.now() + 10_000 });
        setAnswer({ cache: recent, key, attempt, ids });
      } catch (cause) {
        if (!controller.signal.aborted) setAnswer({ cache: recent, key, attempt, error: cause instanceof Error ? cause : new Error(String(cause)) });
      }
    }, 250);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [query, key, filters, networkId, kind, recent, attempt]);

  const current = answer?.cache === recent && answer.key === key && answer.attempt === attempt ? answer : null;
  const visible = useMemo(() => {
    if (!key) return data;
    const ids = current?.ids;
    return ids ? data.filter(row => ids.has("id" in row ? String(row.id) : row.node_uuid)) : [];
  }, [data, current, key]);
  return {
    visible,
    query,
    searchKey: key,
    searchBox: <Input type="search" size="sm" className="w-72 max-w-full" aria-label={label} placeholder={label}
      value={input} onChange={event => {
        if (event.target.value.trim() !== query) setAnswer(null);
        setInput(event.target.value);
      }} />,
    loading: Boolean(key && !current),
    error: current?.error ? new Error(apiErrorText(common, current.error)) : undefined,
    onRefresh: current?.error ? () => setAttempt(value => value + 1) : undefined,
  };
}
