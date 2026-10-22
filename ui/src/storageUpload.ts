// One storage-file upload in a single PUT (ADR 0053). fetch cannot report
// upload progress, so this one request goes through XMLHttpRequest; its error
// answers are read the same way as every other API call's.
import { localizedResponseError, storageFileUploadURL } from "./api";
import type { StorageArchiveFormat, StorageUploadResult } from "./storageTypes";

export interface StorageUploadProgress {
  loaded: number;
  /** The request body's size; the file's size when the browser does not say. */
  total: number;
}

export interface StorageUploadRequest {
  storageId: number;
  /** Required for a local storage; left out for a shared one. */
  node?: string;
  /** The target file, or with extract the directory the archive is unpacked into. */
  path: string;
  file: Blob;
  extract?: StorageArchiveFormat | "";
  overwrite?: boolean;
  signal?: AbortSignal;
  onProgress?: (progress: StorageUploadProgress) => void;
  /** At most one progress report per this many milliseconds; the last byte is always reported. */
  progressIntervalMs?: number;
}

/** The connection broke before an answer came: nothing tells whether the server kept anything. */
export class StorageUploadNetworkError extends Error {
  constructor() {
    super("network error during upload");
    this.name = "StorageUploadNetworkError";
  }
}

/** Whether an error is the upload having been cancelled. */
export function isUploadAbort(error: unknown): boolean {
  return error instanceof Error && error.name === "AbortError";
}

function abortError(): Error {
  const error = new Error("upload cancelled");
  error.name = "AbortError";
  return error;
}

function responseOf(xhr: XMLHttpRequest): Response {
  const headers = new Headers();
  const type = xhr.getResponseHeader("Content-Type");
  if (type) headers.set("Content-Type", type);
  // A Response cannot carry status 0 or a status outside 200–599.
  const status = xhr.status >= 200 && xhr.status <= 599 ? xhr.status : 500;
  return new Response(xhr.responseText || null, { status, statusText: xhr.statusText, headers });
}

/** Sends one file; resolves with what the server put into place, rejects with a localized error, an AbortError or a StorageUploadNetworkError. */
export function uploadStorageFile(request: StorageUploadRequest): Promise<StorageUploadResult> {
  const { signal, onProgress, file } = request;
  const interval = request.progressIntervalMs ?? 200;
  const url = storageFileUploadURL(request.storageId, request.node, request.path, request.extract ?? "", request.overwrite ?? false);
  return new Promise<StorageUploadResult>((resolve, reject) => {
    if (signal?.aborted) {
      reject(abortError());
      return;
    }
    const xhr = new XMLHttpRequest();
    let lastReport = -Infinity;
    const cleanup = () => signal?.removeEventListener("abort", onAbort);
    const onAbort = () => xhr.abort();
    signal?.addEventListener("abort", onAbort);
    xhr.open("PUT", url);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.upload.onprogress = (event: ProgressEvent) => {
      const total = event.lengthComputable && event.total > 0 ? event.total : file.size;
      const now = Date.now();
      if (event.loaded < total && now - lastReport < interval) return;
      lastReport = now;
      onProgress?.({ loaded: event.loaded, total });
    };
    xhr.onload = () => {
      cleanup();
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(JSON.parse(xhr.responseText) as StorageUploadResult);
        } catch {
          reject(new Error(`${url}: ${xhr.status}: unreadable answer`));
        }
        return;
      }
      void localizedResponseError(responseOf(xhr), `${url}: ${xhr.status}`).then(reject, reject);
    };
    xhr.onerror = () => {
      cleanup();
      reject(new StorageUploadNetworkError());
    };
    xhr.onabort = () => {
      cleanup();
      reject(abortError());
    };
    xhr.send(file);
  });
}
