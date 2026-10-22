import { useEffect, useMemo, useState } from "react";
import { api, type Node } from "../api";

/**
 * useNodeLookup fetches the named nodes one by one (GET /nodes/{uuid}) and
 * keeps them: a form that holds a few chosen node uuids reads those nodes,
 * not the whole fleet. A uuid that cannot be read is simply absent from the
 * map; seed rows already in hand (a picker's search page) are used as they
 * are.
 */
export function useNodeLookup(uuids: Array<string | undefined | null>, seed: Node[] = []): Map<string, Node> {
  const [fetched, setFetched] = useState(() => new Map<string, Node>());
  const wanted = [...new Set(uuids.filter((uuid): uuid is string => !!uuid))].sort();
  const key = wanted.join(",");
  const seeded = useMemo(() => new Map(seed.map((node) => [node.uuid, node] as const)), [seed]);
  useEffect(() => {
    const missing = key ? key.split(",").filter((uuid) => !fetched.has(uuid) && !seeded.has(uuid)) : [];
    if (missing.length === 0) return;
    let alive = true;
    void Promise.all(missing.map((uuid) => api.node(uuid).then((node) => [uuid, node] as const).catch(() => null))).then((rows) => {
      if (!alive) return;
      const found = rows.filter((row): row is readonly [string, Node] => row !== null);
      if (found.length) setFetched((current) => new Map([...current, ...found]));
    });
    return () => {
      alive = false;
    };
  }, [key, fetched, seeded]);
  return useMemo(() => {
    const out = new Map<string, Node>();
    for (const uuid of key ? key.split(",") : []) {
      const node = seeded.get(uuid) ?? fetched.get(uuid);
      if (node) out.set(uuid, node);
    }
    return out;
  }, [key, seeded, fetched]);
}
