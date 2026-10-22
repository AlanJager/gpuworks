import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { api } = await import("../src/api.ts");
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

test("node deletion only sends force when the caller explicitly selects it", async () => {
  const requests: { url: string; method?: string }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requests.push({ url: String(input), method: init?.method });
    return new Response('{"status":"deleted","agent_uninstalled":true}', { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  await api.deleteNode("node/one");
  await api.deleteNode("node/one", true);
  assert.deepEqual(requests, [
    { url: "/api/v1/nodes/node%2Fone", method: "DELETE" },
    { url: "/api/v1/nodes/node%2Fone?force=true", method: "DELETE" },
  ]);
});
