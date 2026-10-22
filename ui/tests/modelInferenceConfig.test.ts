import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { test } from "node:test";

// The API module initializes the browser language detector at import time.
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { api } = await import("../src/api.ts");

test("editing a model saves operations and input modalities as one request", async () => {
  const originalFetch = globalThis.fetch;
  let request: { input: RequestInfo | URL; init?: RequestInit } | undefined;
  globalThis.fetch = (async (input, init) => {
    request = { input, init };
    return new Response(JSON.stringify({ id: 7 }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    await api.updateModelInferenceConfig(7, [{ type: "openai.chat" }], ["text", "image"]);
  } finally {
    globalThis.fetch = originalFetch;
  }
  assert.equal(String(request?.input), "/api/v1/models/7/inference-config");
  assert.equal(request?.init?.method, "PATCH");
  assert.deepEqual(JSON.parse(String(request?.init?.body)), {
    operations: [{ type: "openai.chat" }],
    input_modalities: ["text", "image"],
  });
});
