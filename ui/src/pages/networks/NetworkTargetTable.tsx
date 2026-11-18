import { useEffect, useMemo, useState, type ComponentProps } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { Input } from "../../components/ui/input";
import ResourceTable from "../../components/resourceTable/ResourceTable";
import type { NetworkAttachment, NetworkNode } from "../../networkTypes";

type Target = NetworkAttachment | NetworkNode;
type Kind = "nodes" | "attachments";
type Match = { ids: Set<string>; expires: number };
type Answer<T> = { rows: T[]; query: string; attempt: number; ids?: Set<string>; error?: Error };

type Props<T extends Target> = Omit<ComponentProps<typeof ResourceTable<T>>, "data"> & {
  networkId: number;
  kind: Kind;
  data: T[];
};

/** The server matches the full network membership. Rows, actions and paging
 * reuse the already loaded detail, including the unfiltered initial view. */
export default function NetworkTargetTable<T extends Target>({ networkId, kind, data, emptyDescription, ...table }: Props<T>) {
  const { t } = useTranslation("networks");
  const { t: common } = useTranslation("common");
  const [input, setInput] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [answer, setAnswer] = useState<Answer<T> | null>(null);
  const query = input.trim();
  // A new detail snapshot (including an attach/detach) invalidates its matches.
  const recent = useMemo(() => new Map<string, Match>(), [data, networkId, kind]);

  useEffect(() => {
    if (!query) return;
    const cached = recent.get(query);
    if (cached && cached.expires > Date.now()) {
      setAnswer({ rows: data, query, attempt, ids: cached.ids });
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(async () => {
      try {
        const result = await api.searchNetworkTargets(networkId, kind, query, controller.signal);
        if (controller.signal.aborted) return;
        const ids = new Set(result.ids);
        for (const [key, entry] of recent) if (entry.expires <= Date.now()) recent.delete(key);
        recent.set(query, { ids, expires: Date.now() + 10_000 });
        setAnswer({ rows: data, query, attempt, ids });
      } catch (cause) {
        if (!controller.signal.aborted) setAnswer({ rows: data, query, attempt, error: cause instanceof Error ? cause : new Error(String(cause)) });
      }
    }, 250);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [query, networkId, kind, data, recent, attempt]);

  const current = answer?.rows === data && answer.query === query && answer.attempt === attempt ? answer : null;
  const visible = useMemo(() => {
    if (!query) return data;
    const ids = current?.ids;
    return ids ? data.filter(row => ids.has("id" in row ? String(row.id) : row.node_uuid)) : [];
  }, [data, current, query]);
  const label = t(kind === "nodes" ? "detail.searchNodes" : "detail.searchAttachments");
  return <ResourceTable<T> {...table} data={visible} pageResetKey={query}
    toolbar={<Input type="search" size="sm" className="w-72 max-w-full" aria-label={label} placeholder={label}
      value={input} onChange={event => {
        if (event.target.value.trim() !== query) setAnswer(null);
        setInput(event.target.value);
      }} />}
    loading={Boolean(query && !current)} error={current?.error ? new Error(apiErrorText(common, current.error)) : undefined}
    onRefresh={current?.error ? () => setAttempt(value => value + 1) : undefined}
    emptyDescription={query ? t(kind === "nodes" ? "detail.noMatchingNodes" : "detail.noMatchingAttachments") : emptyDescription} />;
}
