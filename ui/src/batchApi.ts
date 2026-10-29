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

export interface Batch {
  id: string;
  endpoint: BatchEndpoint | string;
  status: BatchStatus;
  input_file_id: string;
  /** Only on a single batch (GET, create, cancel), not on list rows. */
  input_filename?: string;
  output_file_id?: string;
  error_file_id?: string;
  completion_window: string;
  request_counts: BatchRequestCounts;
  metadata: Record<string, string>;
  failure?: BatchFailure;
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

/** The limits the create form shows; the server enforces them. */
export const BATCH_LIMITS = { maxFileMB: 200, maxRequests: 50_000 } as const;

/** Output and error files are deleted this long after the batch ended. */
export const BATCH_OUTPUT_RETENTION_DAYS = 30;

/** Seconds-level cadence at which the server checkpoints a running batch's progress. */
export const BATCH_POLL_MS = 5000;

const TERMINAL: ReadonlySet<string> = new Set(["completed", "failed", "expired", "cancelled"]);

/** Whether a batch is still moving: validating, running, finalizing or being cancelled. */
export function isBatchActive(status: string): boolean {
  return !TERMINAL.has(status);
}

/** Whether the owner can still ask for the batch to be cancelled. */
export function isBatchCancellable(status: string): boolean {
  return isBatchActive(status) && status !== "cancelling";
}

/** When the batch ended: the latest of its terminal timestamps, absent while it runs. */
export function batchEndedAt(batch: Batch): string | undefined {
  return batch.completed_at ?? batch.failed_at ?? batch.expired_at ?? batch.cancelled_at;
}

/** The authenticated link of one of a batch's JSON Lines files (the session cookie carries the login). */
export function batchFileURL(id: string, kind: BatchFileKind): string {
  return `/api/v1/batches/${encodeURIComponent(id)}/files/${kind}`;
}

export const batchApi = {
  list: (query: ListQuery) => getJSON<ListPage<Batch>>(`/api/v1/batches?${listQueryString(query)}`),
  get: (id: string) => getJSON<Batch>(`/api/v1/batches/${encodeURIComponent(id)}`),
  /** Uploads the JSONL input and starts the batch (admin only). */
  create: async (file: File, endpoint: string, signal?: AbortSignal): Promise<Batch> => {
    const body = new FormData();
    body.append("file", file, file.name);
    body.append("endpoint", endpoint);
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
