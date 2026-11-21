import type { StorageDownloadFormat } from "../storageTypes";

export interface StorageNodeUsage {
  node_uuid: string;
  hostname: string;
  capacity_known: boolean;
  total_bytes: number;
  available_bytes: number;
  used_bytes: number;
  model_bytes: number;
  image_bytes: number;
  other_bytes: number;
  path_usage?: Array<{
    kind: string;
    /** Names the storage a storage or storage_cache entry measures. */
    storage_id?: number;
    path: string;
    total_bytes: number;
    available_bytes: number;
  }>;
}

/** A storage-files query string: each present value encoded with encodeURIComponent, empty ones left out. */
export function storageFilesQuery(values: Record<string, string | number | boolean | undefined>): string {
  return Object.entries(values)
    .filter(([, value]) => value !== undefined && value !== "")
    .map(([key, value]) => `${key}=${encodeURIComponent(String(value))}`)
    .join("&");
}

/** The link that downloads a storage file as is, or a directory packed as format (zip by default). */
export function storageFileDownloadURL(id: number, node: string | undefined, path: string, format?: StorageDownloadFormat): string {
  return `/api/v1/storages/${id}/files/download?${storageFilesQuery({ node_uuid: node, path, format })}`;
}

/** Where an upload is sent: path is the target file, or the directory an archive is unpacked into. */
export function storageFileUploadURL(id: number, node: string | undefined, path: string, extract: string, overwrite: boolean): string {
  return `/api/v1/storages/${id}/files/upload?${storageFilesQuery({ node_uuid: node, path, extract, overwrite: overwrite ? "true" : undefined })}`;
}
