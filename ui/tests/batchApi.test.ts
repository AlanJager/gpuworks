import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";

// The Batch API calls as the server's contract spells them: method, path,
// query, multipart fields and the product errors that come back.

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const batchApiModule = await import("../src/batchApi.ts");
const { batchApi, batchFileURL, isBatchActive, isBatchCancellable, batchEndedAt, BATCH_ENDPOINTS, BATCH_STATUSES } = batchApiModule;
const { APIError } = await import("../src/apiError.ts");
const { batch, page } = await import("./helpers/batchFixtures.ts");
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

type Seen = { url: string; method?: string; body?: unknown; credentials?: string };

function fake(status: number, body: unknown) {
  const seen: Seen[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push({ url: String(input), method: init?.method, body: init?.body, credentials: init?.credentials });
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return seen;
}

test("list sends the page, page size and the states as one comma list", async () => {
  const seen = fake(200, page([batch()]));
  const got = await batchApi.list({ page: 2, page_size: 50, state: ["completed", "failed"] });
  await batchApi.list({ page: 1, page_size: 20 });
  assert.deepEqual(seen.map((s) => s.url), [
    "/api/v1/batches?page=2&page_size=50&state=completed%2Cfailed",
    "/api/v1/batches?page=1&page_size=20",
  ]);
  assert.equal(got.items[0].id, "batch_aaa111");
  assert.equal(seen[0].credentials, "include");
});

test("get reads one batch by its encoded id", async () => {
  const seen = fake(200, batch({ id: "batch_x/y" }));
  await batchApi.get("batch_x/y");
  assert.equal(seen[0].url, "/api/v1/batches/batch_x%2Fy");
});

test("create posts the file and the endpoint as multipart fields", async () => {
  const seen = fake(201, batch({ status: "validating" }));
  const file = new File(['{"custom_id":"a"}\n'], "reqs.jsonl", { type: "application/x-ndjson" });
  const created = await batchApi.create(file, "/v1/embeddings");
  assert.equal(created.status, "validating");
  assert.equal(seen[0].url, "/api/v1/batches");
  assert.equal(seen[0].method, "POST");
  const form = seen[0].body as FormData;
  assert.ok(form instanceof FormData);
  assert.deepEqual([...form.keys()].sort(), ["endpoint", "file"]);
  assert.equal(form.get("endpoint"), "/v1/embeddings");
  assert.equal((form.get("file") as File).name, "reqs.jsonl");
});

test("create raises the product error with its code and params", async () => {
  fake(400, { error: "line 7: custom_id is missing", code: "BATCH_FILE_INVALID", params: { line: 7, reason: "missing_custom_id", detail: "line 7: custom_id is missing" } });
  const failure = await batchApi.create(new File(["x"], "r.jsonl"), "/v1/completions").catch((e: unknown) => e);
  assert.ok(failure instanceof APIError);
  assert.equal(failure.code, "BATCH_FILE_INVALID");
  assert.equal(failure.status, 400);
  assert.equal(failure.params?.line, 7);
});

test("cancel posts to the cancel path and a finished batch answers BATCH_NOT_CANCELLABLE", async () => {
  const seen = fake(200, batch({ status: "cancelling", cancelling_at: "2026-10-28T09:10:00Z" }));
  const cancelled = await batchApi.cancel("batch_aaa111");
  assert.equal(cancelled.status, "cancelling");
  assert.deepEqual([seen[0].url, seen[0].method], ["/api/v1/batches/batch_aaa111/cancel", "POST"]);
  fake(409, { error: "batch already finished", code: "BATCH_NOT_CANCELLABLE" });
  const failure = await batchApi.cancel("batch_aaa111").catch((e: unknown) => e);
  assert.ok(failure instanceof APIError);
  assert.equal(failure.code, "BATCH_NOT_CANCELLABLE");
  assert.equal(failure.status, 409);
});

test("file links name the kind under the batch", () => {
  assert.equal(batchFileURL("batch_aaa111", "input"), "/api/v1/batches/batch_aaa111/files/input");
  assert.equal(batchFileURL("batch_aaa111", "output"), "/api/v1/batches/batch_aaa111/files/output");
  assert.equal(batchFileURL("batch_aaa111", "error"), "/api/v1/batches/batch_aaa111/files/error");
});

test("a status is active until the batch ended, and cancellable unless already cancelling", () => {
  const active = ["validating", "in_progress", "finalizing", "cancelling"];
  const terminal = ["completed", "failed", "expired", "cancelled"];
  assert.deepEqual([...BATCH_STATUSES].sort(), [...active, ...terminal].sort());
  for (const status of active) assert.equal(isBatchActive(status), true, status);
  for (const status of terminal) assert.equal(isBatchActive(status), false, status);
  assert.deepEqual(active.filter(isBatchCancellable), ["validating", "in_progress", "finalizing"]);
  assert.deepEqual(terminal.filter(isBatchCancellable), []);
});

test("the end of a batch is its terminal timestamp, absent while it runs", () => {
  assert.equal(batchEndedAt(batch()), undefined);
  assert.equal(batchEndedAt(batch({ status: "completed", completed_at: "2026-10-28T10:00:00Z" })), "2026-10-28T10:00:00Z");
  assert.equal(batchEndedAt(batch({ status: "cancelled", cancelled_at: "2026-10-28T10:05:00Z" })), "2026-10-28T10:05:00Z");
  assert.deepEqual([...BATCH_ENDPOINTS], ["/v1/chat/completions", "/v1/completions", "/v1/embeddings"]);
});
