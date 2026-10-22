import type { Storage, StorageFileItem, StorageModelFile, StorageNodeStatus } from "../../src/storageTypes";

const GIB = 1024 ** 3;

/** A directory entry as GET /storages/{id}/files returns it: a writable 12-byte file unless told otherwise. */
export function fileItem(path: string, extra: Partial<StorageFileItem> = {}): StorageFileItem {
  return { name: path.split("/").at(-1) ?? path, path, dir: false, symlink: false, size: 12, mtime: "2026-10-21T08:00:00Z", writable: true, ...extra };
}

export function node(uuid: string, hostname: string, extra: Record<string, unknown> = {}) {
  return { uuid, hostname, name: hostname, state: "online", gpu_count: 1, disk_total_gib: 100, ...extra };
}

export function nodeStatus(uuid: string, name: string, state: string, extra: Partial<StorageNodeStatus> = {}): StorageNodeStatus {
  return { node_uuid: uuid, node_name: name, state, writable: state === "ready", ...extra };
}

export function localStorage(extra: Partial<Storage> = {}): Storage {
  return {
    id: 1, name: "default", type: "local", enabled: true, is_default: true, all_nodes: true, path: "/data/models",
    targets: [], nodes: { total: 2, ready: 2, problems: [] }, writable: true, model_count: 3, deployment_ids: [], ...extra,
  };
}

export function nfsStorage(extra: Partial<Storage> = {}): Storage {
  return {
    id: 2, name: "model-archive", type: "nfs", enabled: true, is_default: false, all_nodes: true,
    path: "/mnt/gpuworks/model-archive", export: "nfs.example.internal:/export/models", mount_options: "vers=4.2",
    total_bytes: 117 * 1024 * GIB, available_bytes: 48 * 1024 * GIB, targets: [],
    nodes: {
      total: 130, ready: 128,
      problems: [
        nodeStatus("n47", "node-47", "failed", { message: "nfs client missing" }),
        nodeStatus("n07", "node-07", "unresponsive", { reason: "storage_unavailable" }),
      ],
    },
    writable: true, model_count: 480, deployment_ids: [4, 9], ...extra,
  };
}

/** A model file row as GET /storages/{id}/model-files returns it: a shared copy unless a node is given. */
export function modelFile(extra: Partial<StorageModelFile> = {}): StorageModelFile {
  return {
    id: 1, model_id: 7, model_name: "Qwen3-8B", source: "modelscope", source_id: "Qwen/Qwen3-8B", path: "Qwen/Qwen3-8B",
    state: "present", acquisition: "registered", size_bytes: 16 * GIB, downloaded: 16 * GIB, progress: 1, deployment_ids: [], ...extra,
  };
}
