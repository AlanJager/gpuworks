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
const { batchApi, batchFileURL, isBatchActive } = batchApiModule;
const { APIError } = await import("../src/apiError.ts");
const { batch, dictionary, page } = await import("./helpers/batchFixtures.ts");
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
  const created = await batchApi.create(file, { endpoint: "/v1/embeddings" });
  assert.equal(created.status, "validating");
  assert.equal(seen[0].url, "/api/v1/batches");
  assert.equal(seen[0].method, "POST");
  const form = seen[0].body as FormData;
  assert.ok(form instanceof FormData);
  assert.deepEqual([...form.keys()].sort(), ["endpoint", "file"]);
  assert.equal(form.get("endpoint"), "/v1/embeddings");
  assert.equal((form.get("file") as File).name, "reqs.jsonl");
  await batchApi.create(file);
  assert.deepEqual([...(seen[1].body as FormData).keys()], ["file"], "without an endpoint the server reads the file's");
});

test("create adds the model field only when a model is chosen", async () => {
  const seen = fake(201, batch({ status: "validating", model: "qwen-chat" }));
  const file = new File(["x"], "reqs.jsonl");
  const created = await batchApi.create(file, { model: "qwen-chat" });
  assert.equal(created.model, "qwen-chat");
  assert.deepEqual([...(seen[0].body as FormData).keys()].sort(), ["file", "model"]);
  assert.equal((seen[0].body as FormData).get("model"), "qwen-chat");
  await batchApi.create(file, { model: "" });
  assert.deepEqual([...(seen[1].body as FormData).keys()].sort(), ["file"]);
});

test("create sends the completion window in hours as completion_window", async () => {
  const seen = fake(201, batch({ status: "validating", completion_window: "72h" }));
  const file = new File(["x"], "reqs.jsonl");
  const created = await batchApi.create(file, { completionWindowHours: 72 });
  assert.equal(created.completion_window, "72h");
  assert.deepEqual([...(seen[0].body as FormData).keys()].sort(), ["completion_window", "file"]);
  assert.equal((seen[0].body as FormData).get("completion_window"), "72h");
});

test("create raises the product error with its code and params", async () => {
  fake(400, { error: "line 7: custom_id is missing", code: "BATCH_FILE_INVALID", params: { line: 7, reason: "missing_custom_id", detail: "line 7: custom_id is missing" } });
  const failure = await batchApi.create(new File(["x"], "r.jsonl")).catch((e: unknown) => e);
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

test("a status is active, and polled, until the batch ended", () => {
  const active = ["validating", "in_progress", "finalizing", "cancelling"];
  const terminal = ["completed", "failed", "expired", "cancelled"];
  for (const status of active) assert.equal(isBatchActive(status), true, status);
  for (const status of terminal) assert.equal(isBatchActive(status), false, status);
});

test("the dictionary is one plain read, returned as the server states it", async () => {
  const wire = dictionary([{ name: "qwen", state: "served" }, { name: "held", state: "asleep_held" }]);
  const seen = fake(200, wire);
  const got = await batchApi.dictionary();
  assert.equal(seen[0].url, "/api/v1/batches/dictionary");
  assert.equal(seen[0].method, undefined);
  assert.deepEqual(got, wire);
  fake(500, { error: "boom" });
  await assert.rejects(batchApi.dictionary());
});
