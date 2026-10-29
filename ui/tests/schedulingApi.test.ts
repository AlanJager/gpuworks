import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";

// The scheduling center's calls, as the server's contract spells them: method,
// path, query and literal JSON bodies.

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

test("schedule calls use the deployment's schedule path and literal bodies", async () => {
  const seen = fake(200, { items: [], total: 0 });
  await api.deploymentSchedule(7);
  await api.createSchedule(7, { action: "sleep", weekdays: [1, 2], time: "22:00", timezone: "", sleep_level: 2, enabled: true });
  await api.updateSchedule(7, 3, { action: "replicas", weekdays: [0], time: "06:30", timezone: "Asia/Shanghai", replicas: 3 });
  assert.deepEqual(seen, [
    { url: "/api/v1/deployments/7/schedule", method: undefined, body: undefined },
    { url: "/api/v1/deployments/7/schedule", method: "POST", body: { action: "sleep", weekdays: [1, 2], time: "22:00", timezone: "", sleep_level: 2, enabled: true } },
    { url: "/api/v1/deployments/7/schedule/3", method: "PUT", body: { action: "replicas", weekdays: [0], time: "06:30", timezone: "Asia/Shanghai", replicas: 3 } },
  ]);
  const deleted = fake(204);
  await api.deleteSchedule(7, 3);
  assert.deepEqual(deleted, [{ url: "/api/v1/deployments/7/schedule/3", method: "DELETE", body: undefined }]);
});

test("idle sleep and node replacement put their seconds", async () => {
  const seen = fake(200, {});
  await api.setIdleSleep(7, { after_sec: 900, level: 2 });
  await api.setNodeReplacement(7, { after_sec: 600 });
  assert.deepEqual(seen, [
    { url: "/api/v1/deployments/7/idle-sleep", method: "PUT", body: { after_sec: 900, level: 2 } },
    { url: "/api/v1/deployments/7/node-replacement", method: "PUT", body: { after_sec: 600 } },
  ]);
});

test("wake carries wait, yield and yield_level only when asked, and a plain wake is unchanged", async () => {
  const seen = fake(202, { status: "waiting", woken: [], waiting: ["u1"], yielded: [] });
  await api.wakeDeployment(7);
  await api.wakeDeployment(7, { partial: true });
  await api.wakeDeployment(7, { role: "prefill" });
  await api.wakeDeployment(7, { wait: true });
  await api.wakeDeployment(7, { yield: true });
  await api.wakeDeployment(7, { yield: true, yieldLevel: 2 });
  await api.wakeInstance("u/1");
  await api.wakeInstance("u/1", { wait: true });
  assert.deepEqual(seen.map((request) => [request.method, request.url]), [
    ["POST", "/api/v1/deployments/7/wake"],
    ["POST", "/api/v1/deployments/7/wake?partial=true"],
    ["POST", "/api/v1/deployments/7/wake?role=prefill"],
    ["POST", "/api/v1/deployments/7/wake?wait=true"],
    ["POST", "/api/v1/deployments/7/wake?yield=true"],
    ["POST", "/api/v1/deployments/7/wake?yield=true&yield_level=2"],
    ["POST", "/api/v1/instances/u%2F1/wake"],
    ["POST", "/api/v1/instances/u%2F1/wake?wait=true"],
  ]);
});

test("cancelling a waiting wake deletes the deployment's wake-waiting", async () => {
  const seen = fake(200, { items: ["u1"], total: 1 });
  const result = await api.endWakeWaiting(7);
  assert.deepEqual(seen, [{ url: "/api/v1/deployments/7/wake-waiting", method: "DELETE", body: undefined }]);
  assert.deepEqual(result, { items: ["u1"], total: 1 });
});

test("the board is asked a page at a time, searched by q", async () => {
  const seen = fake(200, {});
  await api.schedulingBoard({ page: 2, page_size: 10, q: " gpu " });
  await api.schedulingBoard({ page: 1, page_size: 20 });
  assert.deepEqual(seen.map((request) => request.url), [
    "/api/v1/scheduling/board?page=2&page_size=10&q=gpu",
    "/api/v1/scheduling/board?page=1&page_size=20",
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

test("the scheduling history asks for a cursor page with its filters in the query", async () => {
  const seen = fake(200, { items: [] });
  await api.schedulingHistory({});
  await api.schedulingHistory({ limit: 21, before: 90, category: ["auto", "failure"], deployment_id: 7, since: "2026-10-25T00:00:00Z", until: "2026-10-26T00:00:00Z" });
  assert.deepEqual(seen, [
    { url: "/api/v1/scheduling/history", method: undefined, body: undefined },
    { url: "/api/v1/scheduling/history?limit=21&before=90&category=auto%2Cfailure&deployment_id=7&since=2026-10-25T00%3A00%3A00Z&until=2026-10-26T00%3A00%3A00Z", method: undefined, body: undefined },
  ]);
});

test("the residency timeline names every node and the window in the query", async () => {
  const seen = fake(200, { window: { from: "a", to: "b", hours: 24 }, nodes: [], deployments: [] });
  await api.schedulingTimeline({ nodes: ["n-1", "n-2"], hours: 24 });
  assert.deepEqual(seen, [{ url: "/api/v1/scheduling/timeline?node=n-1&node=n-2&hours=24", method: undefined, body: undefined }]);
});
