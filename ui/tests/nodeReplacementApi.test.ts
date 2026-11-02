import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";

// The node replacement and first-placement calls, as the server's contract
// spells them: method, path, query and literal JSON bodies.

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

type Seen = { url: string; method?: string; body?: unknown };

function fake(status = 200, body: unknown = {}) {
  const seen: Seen[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push({ url: String(input), method: init?.method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return seen;
}

test("node replacement puts its seconds", async () => {
  const seen = fake(200, {});
  await api.setNodeReplacement(7, { after_sec: 600 });
  assert.deepEqual(seen, [{ url: "/api/v1/deployments/7/node-replacement", method: "PUT", body: { after_sec: 600 } }]);
});

test("wake carries only partial and role, and an instance wake takes no query", async () => {
  const seen = fake(200, { status: "waking", woken: [] });
  await api.wakeDeployment(7);
  await api.wakeDeployment(7, { partial: true });
  await api.wakeDeployment(7, { role: "prefill" });
  await api.wakeInstance("u/1");
  assert.deepEqual(seen.map((request) => [request.method, request.url]), [
    ["POST", "/api/v1/deployments/7/wake"],
    ["POST", "/api/v1/deployments/7/wake?partial=true"],
    ["POST", "/api/v1/deployments/7/wake?role=prefill"],
    ["POST", "/api/v1/instances/u%2F1/wake"],
  ]);
});

test("scaling may name first-placement nodes, and the server's refusal keeps its code and params", async () => {
  const seen = fake(200, {});
  await api.scaleReplicas(7, { replicas: 3, place_on: ["n-b", "n-c"] });
  assert.deepEqual(seen, [{ url: "/api/v1/deployments/7/replicas", method: "PUT", body: { replicas: 3, place_on: ["n-b", "n-c"] } }]);
  fake(409, { code: "REPLICAS_PLACE_ON_REFUSED", error: "node gpu cannot take a new replica", params: { node: "gpu", reason: "offline" } });
  await assert.rejects(api.scaleReplicas(7, { replicas: 3, place_on: ["n-b"] }), (error: unknown) =>
    error instanceof APIError && error.code === "REPLICAS_PLACE_ON_REFUSED" && error.params?.reason === "offline");
});
