// Why a place in a storage cannot be changed. The server's writable flags are
// the decision; this only picks the sentence that explains a "no".
import {
  isStorageRegionRoot, joinStorageRel, storageRelWritable, type Storage, type StorageFileItem,
} from "../../storageTypes";

export type ReadonlyReason = "disabled" | "region" | "regionRoot" | "other";

/** Why nothing can be created in dir, or null when something can. */
export function dirReadonlyReason(storage: Pick<Storage, "enabled">, dir: string, writable: boolean): ReadonlyReason | null {
  if (writable) return null;
  if (!storage.enabled) return "disabled";
  if (!storageRelWritable(joinStorageRel(dir, "x"))) return "region";
  return "other";
}

/** Why an entry cannot be renamed or deleted, or null when it can. */
export function itemReadonlyReason(storage: Pick<Storage, "enabled">, item: Pick<StorageFileItem, "path" | "writable">): ReadonlyReason | null {
  if (item.writable) return null;
  if (!storage.enabled) return "disabled";
  if (isStorageRegionRoot(item.path)) return "regionRoot";
  if (!storageRelWritable(item.path)) return "region";
  return "other";
}

/** The breadcrumb's steps below the root: each directory on the way to path, with its own path. */
export function pathSteps(path: string): { name: string; path: string }[] {
  if (!path) return [];
  const parts = path.split("/");
  return parts.map((name, index) => ({ name, path: parts.slice(0, index + 1).join("/") }));
}

/** Starts a browser download of url (the session cookie authenticates it). */
export function startDownload(url: string) {
  const link = document.createElement("a");
  link.href = url;
  link.download = "";
  link.rel = "noopener";
  document.body.appendChild(link);
  link.click();
  link.remove();
}
