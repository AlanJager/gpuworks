import { localizedResponseError } from "./api";

// Files to and from the API, beside getJSON and sendJSON in api.ts: a file
// answer saved by the browser, and a multipart form sent with the console
// session. Both decode an error answer the way getJSON does, so a coded
// refusal reaches the page as an APIError in the reader's language instead of
// being saved as the file.

/** DownloadOptions shape one downloadFile call. */
export interface DownloadOptions {
  /** POST when body is given, else GET, unless set. */
  method?: string;
  /** Sent as JSON when present. */
  body?: unknown;
  /** The name to save under when the answer names no file. */
  fallbackName: string;
  signal?: AbortSignal;
}

/**
 * downloadFile asks path with the console session and hands the answer to the
 * browser as a file, named by the answer's Content-Disposition or else
 * fallbackName. It resolves to the name used; an error answer rejects with
 * the localized error and saves nothing.
 */
export async function downloadFile(path: string, { method, body, fallbackName, signal }: DownloadOptions): Promise<string> {
  const response = await fetch(path, {
    method: method ?? (body !== undefined ? "POST" : "GET"),
    credentials: "include",
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  });
  if (!response.ok) throw await localizedResponseError(response, `${path}: ${response.status}`);
  const name = fileNameOf(response.headers.get("Content-Disposition")) ?? fallbackName;
  saveBlob(await response.blob(), name);
  return name;
}

/**
 * saveBlob hands blob to the browser as a download named name. The object URL
 * is released a second later, once the browser has begun reading it.
 */
export function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const revoke = URL.revokeObjectURL.bind(URL);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  document.body.append(link);
  link.click();
  link.remove();
  // Keep cleanup with the API that owns the URL, even if a page replaces it.
  setTimeout(() => revoke(url), 1000);
}

/**
 * fileNameOf reads the file name from a Content-Disposition header: the
 * UTF-8 filename* form first (RFC 6266), then the plain one. Any directory
 * part is dropped; no usable name gives undefined.
 */
export function fileNameOf(header: string | null): string | undefined {
  if (!header) return undefined;
  const extended = /filename\*\s*=\s*(?:"?)[^']*'[^']*'([^";]+)/i.exec(header);
  if (extended) {
    try {
      const name = baseName(decodeURIComponent(extended[1].trim()));
      if (name) return name;
    } catch {
      // A malformed escape: fall back to the plain form.
    }
  }
  const plain = /filename\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^;]+))/i.exec(header);
  if (!plain) return undefined;
  const raw = plain[1] !== undefined ? plain[1].replace(/\\(.)/g, "$1") : plain[2].trim();
  return baseName(raw);
}

function baseName(name: string): string | undefined {
  const last = name.split(/[\\/]/).pop()?.trim();
  return last && last !== "." && last !== ".." ? last : undefined;
}

/**
 * sendForm sends a multipart form (files and fields) with the console session
 * and reads the JSON answer; an empty answer gives undefined. The browser
 * writes the multipart Content-Type with its boundary. Errors decode as in
 * sendJSON.
 */
export async function sendForm<T>(method: string, path: string, form: FormData, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, { method, credentials: "include", body: form, signal });
  if (!response.ok) throw await localizedResponseError(response, `${path}: ${response.status}`);
  const text = await response.text();
  return text ? (JSON.parse(text) as T) : (undefined as T);
}
