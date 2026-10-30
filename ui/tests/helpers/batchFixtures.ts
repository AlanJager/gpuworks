import type { Batch } from "../../src/batchApi.ts";

/** A batch as the management API returns it, overridden per test. */
export function batch(overrides: Partial<Batch> = {}): Batch {
  return {
    id: "batch_aaa111",
    endpoint: "/v1/chat/completions",
    status: "in_progress",
    input_file_id: "file-in1",
    completion_window: "24h",
    request_counts: { total: 10, completed: 4, failed: 1 },
    metadata: {},
    user_id: 3,
    api_key_id: 8,
    created_at: "2026-10-28T09:00:00Z",
    expires_at: "2026-10-29T09:00:00Z",
    in_progress_at: "2026-10-28T09:00:05Z",
    ...overrides,
  };
}

export function page(items: Batch[], extra: { page?: number; page_size?: number; total?: number } = {}) {
  return { items, page: extra.page ?? 1, page_size: extra.page_size ?? 20, total: extra.total ?? items.length };
}
