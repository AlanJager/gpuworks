import { isModelFileReady, type Storage, type StorageModelFile, type StorageTarget } from "../../storageTypes";

/** Bytes of a model file that are on disk now: all of it once finished, what has arrived so far otherwise. */
export function fileBytes(file: StorageModelFile): number {
  return isModelFileReady(file.state) ? file.size_bytes : file.downloaded;
}

/** How much model data each node's cache holds, from the cache rows of an nfs_cache storage. */
export function cacheBytesByNode(files: StorageModelFile[]): Map<string, number> {
  const bytes = new Map<string, number>();
  for (const file of files) {
    if (!file.cache || !file.node_uuid) continue;
    bytes.set(file.node_uuid, (bytes.get(file.node_uuid) ?? 0) + fileBytes(file));
  }
  return bytes;
}

/** The per-node settings a storage carries for a node, if any. */
export function targetOf(storage: Storage, nodeUUID: string): StorageTarget | undefined {
  return storage.targets.find((x) => x.node_uuid === nodeUUID);
}

/** A node's cache limit: its own override when set, else the storage's; 0 means no limit. */
export function cacheLimitOf(storage: Storage, nodeUUID: string): number {
  return targetOf(storage, nodeUUID)?.cache_max_bytes ?? storage.cache_max_bytes ?? 0;
}

/** Stable key of a row: one model's file in one place. */
export function fileKey(file: StorageModelFile): string {
  return `${file.id}:${file.model_id}:${file.node_uuid ?? "shared"}:${file.cache ? "c" : "l"}`;
}

export type FileScope = "all" | "shared" | string;

/** Rows for a node/shared choice and a model-name search. */
export function filterFiles(files: StorageModelFile[], scope: FileScope, search: string): StorageModelFile[] {
  const needle = search.trim().toLowerCase();
  return files.filter((file) => {
    if (scope === "shared" && file.node_uuid) return false;
    if (scope !== "all" && scope !== "shared" && file.node_uuid !== scope) return false;
    return !needle || (file.model_name || file.source_id).toLowerCase().includes(needle);
  });
}
