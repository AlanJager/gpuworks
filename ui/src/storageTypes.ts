// Wire types for /api/v1/storages. They mirror internal/server/api/dto/storage.go;
// a storage's type values are the server's (local, nfs, nfs_cache, shared_dir).

import type { ProductMessage } from "./apiError";

export type StorageType = "local" | "nfs" | "nfs_cache" | "shared_dir";

/** What a storage type means, in one place: the pages ask this instead of comparing type strings. */
export interface StorageTypeInfo {
  /** Model files exist once, whatever the number of nodes. */
  shared: boolean;
  /** Mounted from an NFS export (has an export address and mount options). */
  nfs: boolean;
  /** Each node keeps a local cache of the shared files. */
  cache: boolean;
  /** The other NFS type an admin can switch it to. */
  switchTo?: StorageType;
}

const TYPE_INFO: Record<StorageType, StorageTypeInfo> = {
  local: { shared: false, nfs: false, cache: false },
  nfs: { shared: true, nfs: true, cache: false, switchTo: "nfs_cache" },
  nfs_cache: { shared: true, nfs: true, cache: true, switchTo: "nfs" },
  shared_dir: { shared: true, nfs: false, cache: false },
};

const NO_INFO: StorageTypeInfo = { shared: false, nfs: false, cache: false };

/** Every storage type, in the order the form offers them. */
export const STORAGE_TYPES = Object.keys(TYPE_INFO) as StorageType[];

/** The facts about a storage type; an unknown or missing type has none of them. */
export function storageTypeInfo(type?: string): StorageTypeInfo {
  return (type && (TYPE_INFO as Record<string, StorageTypeInfo | undefined>)[type]) || NO_INFO;
}

export interface StorageTarget {
  node_uuid?: string;
  node_name?: string;
  label_key?: string;
  label_value?: string;
  path?: string;
  cache_path?: string;
  cache_max_bytes?: number;
  existing?: boolean;
  /** An exception alone does not add a node to the storage's scope. */
  override_only?: boolean;
}

export interface StorageNodeCache {
  used_bytes: number;
  reclaimable_bytes: number;
  protected_bytes: number;
  limit_bytes: number;
  state: string;
  reason?: string;
  message?: string;
}

/** Missing fields stay unchanged; null restores inheritance; zero is unlimited. */
export interface StorageNodeOverrideRequest {
  node_uuids: string[];
  cache_path?: string | null;
  cache_max_bytes?: number | null;
}

export interface StorageNodeStatus {
  node_uuid: string;
  node_name: string;
  /** ready | mounting | failed | unresponsive | unsupported | offline */
  state: string;
  reason?: string;
  message?: string;
  path?: string;
  cache_path?: string;
  writable: boolean;
  /** Capacity this node last measured for the storage: its own disk for a local storage, the shared volume otherwise. */
  total_bytes?: number;
  available_bytes?: number;
  /** The node's cache disk; only an nfs_cache storage has one. */
  cache_total_bytes?: number;
  cache_available_bytes?: number;
  updated_at?: string;
  /** The node cannot use the storage's directory and needs one of its own (the server decides). */
  needs_own_directory?: boolean;
  cache?: StorageNodeCache;
}

export interface StorageNodes {
  total: number;
  ready: number;
  problems: StorageNodeStatus[];
  all?: StorageNodeStatus[];
}

export interface Storage {
  id: number;
  name: string;
  type: StorageType | string;
  enabled: boolean;
  is_default: boolean;
  all_nodes: boolean;
  path: string;
  export?: string;
  mount_options?: string;
  cache_path?: string;
  cache_max_bytes?: number;
  total_bytes?: number;
  available_bytes?: number;
  targets: StorageTarget[];
  nodes: StorageNodes;
  /** Whether any node can write to it (local storages: always). */
  writable: boolean;
  model_count: number;
  deployment_ids: number[];
  updated_at?: string;
}

/** Writable means the server accepts new model files: enabled, and some node can write to it. */
export function canWriteStorage(storage: Pick<Storage, "enabled" | "writable">): boolean {
  return storage.enabled && storage.writable;
}

/** Create or change a storage; on a change, absent fields are left alone. */
export interface StorageRequest {
  name?: string;
  type?: string;
  enabled?: boolean;
  is_default?: boolean;
  all_nodes?: boolean;
  path?: string;
  export?: string;
  mount_options?: string;
  cache_path?: string;
  cache_max_bytes?: number;
  targets?: StorageTarget[];
  probe_node?: string;
  expected_updated_at?: string;
}

export interface StorageProbeRequest {
  node_uuid?: string;
  /** The storage's name: a shared storage without a path is tried at the server's default mount point for it. */
  name?: string;
  type: string;
  path: string;
  export?: string;
  mount_options?: string;
  existing?: boolean;
}

export interface StorageProbeResult {
  node_uuid: string;
  node_name: string;
  ok: boolean;
  reason?: string;
  message?: string;
  output?: string;
  version?: string;
  options?: string;
  readable: boolean;
  writable: boolean;
  total_bytes?: number;
  available_bytes?: number;
}

export interface StorageScanCandidate {
  rel_path: string;
  size_bytes: number;
  files: number;
  source: string;
  source_id: string;
  revision?: string;
  subfolder?: string;
  format?: string;
  tasks?: string[];
  metadata_issues?: string[];
  /** How the identity was recognised: gpuworks marker, tool metadata, or path only. */
  basis: string;
  incomplete?: boolean;
  incomplete_reason?: string;
  imported: boolean;
  bind_model_id?: number;
  bind_model_name?: string;
}

export interface StorageScanSkipped {
  rel_path: string;
  reason: string;
}

export interface StorageScanResult {
  node_uuid: string;
  candidates: StorageScanCandidate[];
  skipped: StorageScanSkipped[];
  next_cursor?: string;
  complete: boolean;
}

export interface StorageImportItem {
  rel_path: string;
  model_id?: number;
  source?: string;
  source_id?: string;
  revision?: string;
  subfolder?: string;
  name?: string;
  size_bytes?: number;
}

export interface StorageImported {
  rel_path: string;
  model_id?: number;
  created?: boolean;
  error?: string;
  error_detail?: ProductMessage;
}

export interface StorageImportResult {
  imported: StorageImported[];
  failed: StorageImported[];
}

/** Another gpuworks installation using the same shared storage. */
export interface StorageControlPlane {
  id: string;
  name: string;
  version?: string;
  last_seen_seconds: number;
  fresh: boolean;
}

export interface StorageSettings {
  control_plane_id: string;
  control_plane_name: string;
  reserve_free_bytes: number;
  /** Where a storage's directories go when its definition leaves them out (the server applies these). */
  defaults: StorageDefaults;
}

/** A local storage's default directory, and the roots a mount point and a node cache are made under (<root>/<name>). */
export interface StorageDefaults {
  local_path: string;
  mount_root: string;
  cache_root: string;
}

/** A model file a deployment or a copy can read from: finished, whatever way it got there. */
export function isModelFileReady(state: string): boolean {
  return state === "present" || state === "registered";
}

/** A model's single copy in a shared storage (dto.ModelStoragePresence). */
export interface ModelStoragePresence {
  storage_id: number;
  storage_name: string;
  storage_type: string;
  path: string;
  state: string;
  acquisition: string;
  message?: string;
  revision?: string;
  size_bytes: number;
  downloaded: number;
  progress: number;
  rate_bps: number;
  ready_nodes: number;
  cached_nodes: number;
  writer_node?: string;
  /** Whether this gpuworks may change the storage now: a read-only (disabled) one keeps its files. */
  writable: boolean;
}

/**
 * One model's files in one place of a storage (GET /storages/{id}/model-files).
 * A shared storage has one row without a node for the single shared copy; an
 * nfs_cache storage also has a cache row per node; a local storage has a row
 * per node holding the model.
 */
export interface StorageModelFile {
  id: number;
  model_id: number;
  model_name: string;
  source: string;
  source_id: string;
  node_uuid?: string;
  node_name?: string;
  /** A node's cache of an nfs_cache storage. */
  cache?: boolean;
  path: string;
  state: string;
  message?: string;
  acquisition?: string;
  size_bytes: number;
  downloaded: number;
  progress: number;
  writer_node?: string;
  last_used_at?: string;
  deployment_ids: number[];
}

/** The pieces of a copy/download request that name a location. */
export interface ModelLocationRef {
  storage_id: number;
  node_uuid?: string;
}

// Storage files (ADR 0053): browse a whole storage; write only inside its
// writable regions. The server decides what is writable; the pages use the
// region helpers only to say why something is not.

/** One entry of a storage directory (GET /storages/{id}/files), read without following links. */
export interface StorageFileItem {
  name: string;
  /** Storage-relative slash path. */
  path: string;
  dir: boolean;
  /** A symbolic link: listed, never followed. */
  symlink?: boolean;
  size: number;
  mtime: string;
  /** Whether it can be renamed or deleted now. */
  writable: boolean;
}

/** One page of a storage directory. */
export interface StorageFileList {
  /** The directory, storage-relative ("" is the root). */
  path: string;
  /** The node that read it; the server picks one for a shared storage. */
  node_uuid?: string;
  /** Whether something can be created in this directory now. */
  writable: boolean;
  items: StorageFileItem[];
  /** The cursor of the next page; empty on the last one. */
  next_cursor?: string;
}

/** What one upload put into place: the file, or the directory an archive was unpacked into. */
export interface StorageUploadResult {
  path: string;
  bytes: number;
  files: number;
}

/** What an upload may be unpacked from. */
export type StorageArchiveFormat = "tar" | "tar.gz" | "zip";
/** What a directory download is packed into; zip when absent. */
export type StorageDownloadFormat = "zip" | "tar.gz";

/** The regions of a storage open to writes (proto.StorageWritableRegions). */
export const STORAGE_WRITABLE_REGIONS = ["files", ".gpuworks/apps"] as const;

/** Whether a storage-relative path lies strictly inside a writable region (proto.StorageRelWritable). */
export function storageRelWritable(path: string): boolean {
  return STORAGE_WRITABLE_REGIONS.some((region) => path.startsWith(`${region}/`) && path.length > region.length + 1);
}

/** Whether a path is a writable region's own directory: things go into it, but it cannot be renamed or deleted. */
export function isStorageRegionRoot(path: string): boolean {
  return (STORAGE_WRITABLE_REGIONS as readonly string[]).includes(path);
}

/** The archive format a file name says it is in, if any. */
export function archiveFormatOf(name: string): StorageArchiveFormat | undefined {
  if (/\.(tar\.gz|tgz)$/i.test(name)) return "tar.gz";
  if (/\.tar$/i.test(name)) return "tar";
  if (/\.zip$/i.test(name)) return "zip";
  return undefined;
}

/** A file name without its archive extension: the directory an archive is unpacked into. */
export function archiveStem(name: string): string {
  return name.replace(/\.(tar\.gz|tgz|tar|zip)$/i, "") || name;
}

/** Joins a directory and a name into a storage-relative path. */
export function joinStorageRel(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

/** The directory a storage-relative path is in ("" for the root's entries). */
export function storageRelParent(path: string): string {
  const at = path.lastIndexOf("/");
  return at < 0 ? "" : path.slice(0, at);
}

/** A name a person typed for a new entry: one path segment, not "." or "..". */
export function validEntryName(name: string): boolean {
  return name !== "" && name !== "." && name !== ".." && !name.includes("/") && !name.includes("\\") && !name.includes("\0");
}
