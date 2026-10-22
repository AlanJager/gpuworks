import type { Node } from "../../api";
import type { Storage, StorageNodeStatus, StorageTarget, StorageNodeOverrideRequest } from "../../storageTypes";
import { cacheLimitOf } from "./storageFiles";

export type CacheNodeRow = StorageNodeStatus & { active: boolean; target?: StorageTarget; node?: Node };
export type CachePolicyMode = "inherit" | "custom" | "unlimited";

export function cachePolicyMode(target?: StorageTarget): CachePolicyMode {
  return target?.cache_max_bytes == null ? "inherit" : target.cache_max_bytes === 0 ? "unlimited" : "custom";
}

/** Inactive exceptions stay visible without making the node a storage member. */
export function cacheNodeRows(storage: Storage, nodes: Node[]): CacheNodeRow[] {
  const targets = new Map(storage.targets.filter((x) => x.node_uuid).map((x) => [x.node_uuid!, x]));
  const inventory = new Map(nodes.map((x) => [x.uuid, x]));
  const rows: CacheNodeRow[] = (storage.nodes.all ?? []).map((row) => ({ ...row, active: true, target: targets.get(row.node_uuid), node: inventory.get(row.node_uuid) }));
  if (!storage.nodes.all && storage.nodes.total > 0) return rows;
  const active = new Set(rows.map((x) => x.node_uuid));
  for (const [uuid, target] of targets) {
    if (active.has(uuid) || !target.override_only) continue;
    const node = inventory.get(uuid);
    rows.push({ node_uuid: uuid, node_name: target.node_name || node?.name || node?.hostname || uuid, state: "inactive", writable: false, active: false, target, node });
  }
  return rows;
}

export function cacheRowState(row: CacheNodeRow): string {
  if (!row.active) return "inactive";
  if (row.state === "offline") return "offline";
  return row.cache?.state ?? "unknown";
}

export function filterCacheRows(rows: CacheNodeRow[], search: string, modes: string[], states: string[]): CacheNodeRow[] {
  const needle = search.trim().toLowerCase();
  return rows.filter((row) => {
    const text = [row.node_name, row.node_uuid, row.node?.hostname, row.node?.management_address].join(" ").toLowerCase();
    return (!needle || text.includes(needle)) && (!modes.length || modes.includes(cachePolicyMode(row.target))) && (!states.length || states.includes(cacheRowState(row)));
  });
}

/** Fixed selection and only fields the operator explicitly chose to change. */
export function cacheOverrideRequest(ids: string[], changeLimit: boolean, limit: number | undefined, pathMode: "keep" | "inherit" | "custom", path: string): StorageNodeOverrideRequest {
  return {
    node_uuids: [...ids],
    ...(changeLimit ? { cache_max_bytes: limit ?? null } : {}),
    ...(pathMode === "inherit" ? { cache_path: null } : pathMode === "custom" ? { cache_path: path.trim() } : {}),
  };
}

export function cachePolicyImpact(storage: Storage, rows: CacheNodeRow[], limit: number | undefined, changeLimit: boolean, defaults = false, nextPath?: { mode: "inherit" | "custom"; value?: string }) {
  const affected = defaults ? rows.filter((row) => row.active && ((changeLimit && cachePolicyMode(row.target) === "inherit") || (nextPath && !row.target?.cache_path))) : rows;
  let over = 0, blocked = 0, unknown = 0, offline = 0;
  for (const row of affected) {
    if (row.state === "offline") offline++;
    const currentPath = row.target?.cache_path || storage.cache_path;
    const proposedPath = nextPath ? (defaults && row.target?.cache_path ? row.target.cache_path : nextPath.mode === "inherit" ? storage.cache_path : nextPath.value) : currentPath;
    if (!row.cache || !row.active || row.state === "offline" || proposedPath !== currentPath || (!!row.cache_path && row.cache_path !== currentPath)) { unknown++; continue; }
    const next = !changeLimit ? cacheLimitOf(storage, row.node_uuid) : defaults ? (limit ?? 0) : (limit ?? storage.cache_max_bytes ?? 0);
    if (next > 0 && row.cache.used_bytes > next) over++;
    if (next > 0 && row.cache.protected_bytes > next) blocked++;
  }
  return { count: affected.length, over, blocked, unknown, offline };
}

/** A changed cache directory cannot borrow the old directory's capacity. */
export function cacheDiskKnown(storage: Storage, row: CacheNodeRow): boolean {
  const path = row.target?.cache_path || storage.cache_path;
  return row.active && row.state !== "offline" && !!path && row.cache_path === path && (row.cache_total_bytes ?? 0) > 0;
}
