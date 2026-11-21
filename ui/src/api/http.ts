import i18n from "../i18n";
import { APIError, apiErrorText, apiResponseError, ProxyErrorPage } from "../apiError";

// Every collection endpoint answers an envelope, never a bare array
// (docs/reference/api-conventions.md). List is a whole, unpaged collection:
// total equals items.length.
export interface List<T> {
  items: T[];
  total: number;
}

// CursorPage is one page of a cursor-paged collection (the event timelines):
// the next page is asked for with a cursor taken from the last item, and no
// total is counted.
export interface CursorPage<T> {
  items: T[];
}

// One page of a server-paged list (GET /deployments, GET /nodes with page
// parameters). total counts every row the filters match.
export interface ListPage<T> extends List<T> {
  page: number;
  page_size: number;
}

export interface ListQuery {
  page: number;
  page_size: number;
  /** Matched as a case-insensitive substring of the row's names. */
  q?: string;
  /** Alternatives; empty matches every state. */
  state?: string[];
  /** key=value pairs: values of one key are alternatives, keys must all match. */
  label?: string[];
  /** A column key, ascending; "-" before it for descending. */
  sort?: string;
  /** A list's own column filters (source, vendor, allocation_state…): each lists alternatives. */
  extra?: Record<string, string[]>;
}

export function listQueryString(query: ListQuery): string {
  const params = new URLSearchParams({ page: String(query.page), page_size: String(query.page_size) });
  const q = query.q?.trim();
  if (q) params.set("q", q);
  if (query.state?.length) params.set("state", query.state.join(","));
  for (const pair of query.label ?? []) params.append("label", pair);
  if (query.sort) params.set("sort", query.sort);
  for (const [name, values] of Object.entries(query.extra ?? {})) if (values.length) params.set(name, values.join(","));
  return params.toString();
}

/** getJSON reads a JSON answer, an error answer as a localized Error. */
export async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, { signal, credentials: "include" });
  if (!res.ok)
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  return res.json();
}

export async function localizedResponseError(
  response: Response,
  fallback: string,
): Promise<Error> {
  const error = await apiResponseError(response, fallback);
  if (error instanceof APIError) {
    error.message = apiErrorText((key, options) => i18n.t(key, options), error);
  } else if (error instanceof ProxyErrorPage) {
    error.message = i18n.t("httpError.proxyPage", { status: error.status, defaultValue: error.message });
  }
  return error;
}

export async function sendJSON<T>(
  method: string,
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "include",
    headers:
      body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  });
  if (!res.ok) {
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  }
  const text = await res.text();
  return text ? (JSON.parse(text) as T) : (undefined as T);
}
