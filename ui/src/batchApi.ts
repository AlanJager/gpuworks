// Client for the Batch API management endpoints (GET/POST /api/v1/batches,
// docs/reference/api.md "Batches"). The wire types mirror the server's batch
// DTO; times are RFC3339 strings and the ones a batch has not reached are
// absent.
import { getJSON, listQueryString, localizedResponseError, type ListPage, type ListQuery } from "./api";

export type BatchEndpoint = "/v1/chat/completions" | "/v1/completions" | "/v1/embeddings";
export const BATCH_ENDPOINTS: readonly BatchEndpoint[] = ["/v1/chat/completions", "/v1/completions", "/v1/embeddings"];

export type BatchStatus =
  | "validating"
  | "in_progress"
  | "finalizing"
  | "completed"
  | "failed"
  | "expired"
  | "cancelling"
  | "cancelled";
export const BATCH_STATUSES: readonly BatchStatus[] = [
  "validating", "in_progress", "finalizing", "cancelling", "completed", "failed", "expired", "cancelled",
];

export type BatchFileKind = "input" | "output" | "error";

/** Requests that reached a final result; requests still being retried are in neither count. */
export interface BatchRequestCounts {
  total: number;
  completed: number;
  failed: number;
}

export interface BatchFailure {
  code: string;
  message: string;
}

/**
 * A finding that does not refuse the batch: BATCH_MODEL_SLEEP_HELD {model} says the model the batch was given sleeps by an
 * operator's order, which requests do not wake; BATCH_FILE_MODELS_SLEEP_HELD {models, more} says so of models its file names.
 */
export interface BatchWarning {
  code: string;
  params?: Record<string, unknown>;
}

export interface Batch {
  id: string;
  endpoint: BatchEndpoint | string;
  status: BatchStatus;
  /** Whether a cancel would stop the batch now; the server decides (a finalizing batch, for one, ends as it is). */
  cancellable: boolean;
  input_file_id: string;
  /** Only on a single batch (GET, create, cancel), not on list rows. */
  input_filename?: string;
  output_file_id?: string;
  error_file_id?: string;
  completion_window: string;
  /** The served name every request is sent to, replacing its line's model; absent when each line names its own. */
  model?: string;
  request_counts: BatchRequestCounts;
  metadata: Record<string, string>;
  failure?: BatchFailure;
  /** When the output and error files are deleted, the server's retention after the batch ended; absent while it runs or without files. */
  output_expires_at?: string;
  /** Non-blocking findings of creating the batch; only on the create response. */
  warnings?: BatchWarning[];
  user_id: number;
  api_key_id: number;
  created_at: string;
  expires_at: string;
  in_progress_at?: string;
  finalizing_at?: string;
  completed_at?: string;
  failed_at?: string;
  expired_at?: string;
  cancelling_at?: string;
  cancelled_at?: string;
}

/** What the create call takes besides the file and the endpoint. */
export interface BatchCreateOptions {
  model?: string;
  completionWindowHours?: number;
  signal?: AbortSignal;
}

/** The completion windows a batch may ask for, in whole hours. */
export const BATCH_WINDOW_HOURS = { min: 1, max: 72, default: 24 } as const;

/** The limits the create form shows; the server enforces them. */
export const BATCH_LIMITS = { maxFileMB: 200, maxRequests: 50_000 } as const;

/** Seconds-level cadence at which the server checkpoints a running batch's progress. */
export const BATCH_POLL_MS = 5000;

const TERMINAL: ReadonlySet<string> = new Set(["completed", "failed", "expired", "cancelled"]);

/** Whether a batch is still moving (validating, running, finalizing or being cancelled), so the page keeps polling it. */
export function isBatchActive(status: string): boolean {
  return !TERMINAL.has(status);
}

/** The authenticated link of one of a batch's JSON Lines files (the session cookie carries the login). */
export function batchFileURL(id: string, kind: BatchFileKind): string {
  return `/api/v1/batches/${encodeURIComponent(id)}/files/${kind}`;
}

export const batchApi = {
  list: (query: ListQuery) => getJSON<ListPage<Batch>>(`/api/v1/batches?${listQueryString(query)}`),
  get: (id: string) => getJSON<Batch>(`/api/v1/batches/${encodeURIComponent(id)}`),
  /**
   * Uploads the JSONL input and starts the batch (admin only). model, when
   * given, replaces the model of every line; completionWindowHours is the time
   * the batch has (the server's default is 24, BATCH_WINDOW_HOURS the range).
   */
  create: async (file: File, endpoint: string, options: BatchCreateOptions = {}): Promise<Batch> => {
    const { model, completionWindowHours, signal } = options;
    const body = new FormData();
    body.append("file", file, file.name);
    body.append("endpoint", endpoint);
    if (model) body.append("model", model);
    if (completionWindowHours !== undefined) body.append("completion_window", `${completionWindowHours}h`);
    const res = await fetch("/api/v1/batches", { method: "POST", credentials: "include", body, signal });
    if (!res.ok) throw await localizedResponseError(res, `/api/v1/batches: ${res.status}`);
    return res.json();
  },
  /** Asks the executor to stop the batch; it shows as cancelling until in-flight requests end (admin only). */
  cancel: async (id: string): Promise<Batch> => {
    const path = `/api/v1/batches/${encodeURIComponent(id)}/cancel`;
    const res = await fetch(path, { method: "POST", credentials: "include" });
    if (!res.ok) throw await localizedResponseError(res, `${path}: ${res.status}`);
    return res.json();
  },
};
