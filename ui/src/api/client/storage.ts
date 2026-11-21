import type { Storage, StorageControlPlane, StorageImportItem, StorageImportResult, StorageModelFile, StorageProbeRequest, StorageProbeResult, StorageRequest, StorageScanResult, StorageSettings, StorageNodeOverrideRequest, StorageFileList } from "../../storageTypes";
import { getJSON, sendJSON, type List } from "../http";
import { storageFilesQuery, type StorageNodeUsage } from "../storage";

export const storageApi = {
  storages: () => getJSON<List<Storage>>("/api/v1/storages"),
  storage: (id: number, allNodes = false) =>
    getJSON<Storage>(`/api/v1/storages/${id}${allNodes ? "?all_nodes=1" : ""}`),
  storageUsage: () => getJSON<List<StorageNodeUsage>>("/api/v1/storages/usage"),
  /** The model files in a storage: the shared copy, node caches, or the node copies of a local storage. */
  storageModelFiles: (id: number) => getJSON<List<StorageModelFile>>(`/api/v1/storages/${id}/model-files`),
  createStorage: (body: StorageRequest) => sendJSON<Storage>("POST", "/api/v1/storages", body),
  updateStorage: (id: number, body: StorageRequest) => sendJSON<Storage>("PATCH", `/api/v1/storages/${id}`, body),
  updateStorageNodeOverrides: (id: number, body: StorageNodeOverrideRequest) =>
    sendJSON<Storage>("PATCH", `/api/v1/storages/${id}/node-overrides`, body),
  reconcileStorageCache: (id: number, nodeUUIDs: string[], mode: "trim" | "clear") =>
    sendJSON<unknown>("POST", `/api/v1/storages/${id}/cache/reconcile`, { node_uuids: nodeUUIDs, mode }),
  deleteStorage: (id: number) => sendJSON<void>("DELETE", `/api/v1/storages/${id}`),
  probeStorage: (body: StorageProbeRequest) =>
    sendJSON<StorageProbeResult>("POST", "/api/v1/storages/probe", body),
  storageControlPlanes: (id: number) =>
    getJSON<List<StorageControlPlane>>(`/api/v1/storages/${id}/control-planes`),
  scanStorage: (id: number, body: { cursor?: string; limit?: number }) =>
    sendJSON<StorageScanResult>("POST", `/api/v1/storages/${id}/scan`, body),
  importStorage: (id: number, items: StorageImportItem[]) =>
    sendJSON<StorageImportResult>("POST", `/api/v1/storages/${id}/import`, { items }),
  clearStorageCache: (id: number, nodeUuid?: string) =>
    sendJSON<{ removed: number; freed_bytes: number }>(
      "DELETE",
      `/api/v1/storages/${id}/cache${nodeUuid ? `?node_uuid=${encodeURIComponent(nodeUuid)}` : ""}`,
    ),
  storageSettings: () => getJSON<StorageSettings>("/api/v1/storages/settings"),
  updateStorageSettings: (body: { control_plane_name?: string; reserve_free_bytes?: number }) =>
    sendJSON<StorageSettings>("PATCH", "/api/v1/storages/settings", body),
  // Storage files (ADR 0053). node is required for a local storage and left
  // out for a shared one, where the server picks a node. Uploads go through
  // storageUpload.ts (XHR, for progress); downloads are plain links.
  storageFiles: (id: number, query: { node?: string; path: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    getJSON<StorageFileList>(`/api/v1/storages/${id}/files?${storageFilesQuery({ node_uuid: query.node, path: query.path, cursor: query.cursor, limit: query.limit })}`, signal),
  storageFilesMkdir: (id: number, node: string | undefined, path: string) =>
    sendJSON<{ path: string }>("POST", `/api/v1/storages/${id}/files/mkdir`, { ...(node ? { node_uuid: node } : {}), path }),
  storageFilesRename: (id: number, node: string | undefined, from: string, to: string) =>
    sendJSON<{ path: string }>("POST", `/api/v1/storages/${id}/files/rename`, { ...(node ? { node_uuid: node } : {}), from, to }),
  deleteStorageFile: (id: number, node: string | undefined, path: string) =>
    sendJSON<void>("DELETE", `/api/v1/storages/${id}/files?${storageFilesQuery({ node_uuid: node, path })}`),
};
