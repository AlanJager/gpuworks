import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";

// api.ts pulls in src/i18n (i18next + the browser language detector), which
// needs a DOM to initialize against - the same minimal globals every other
// test that imports the real `api` object sets up before doing so.
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}

const { api } = await import("../src/api.ts");

const originalFetch = globalThis.fetch;
let lastURL = "";

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function captureURL() {
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    lastURL = String(input);
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
}

test("logsSearch serializes instances as a comma-separated list, mutually exclusive with instance", async () => {
  captureURL();
  await api.logsSearch({ instances: ["inst-a", "inst-b", "inst-c"], limit: 2000, from: 1000, to: 2000 });
  const url = new URL(lastURL, "http://localhost");
  assert.equal(url.searchParams.get("instances"), "inst-a,inst-b,inst-c");
  assert.equal(url.searchParams.get("instance"), null);
  assert.equal(url.searchParams.get("limit"), "2000");
  assert.equal(url.searchParams.get("from"), "1000");
  assert.equal(url.searchParams.get("to"), "2000");
});

test("logsSearch omits instances entirely when the list is empty, and still supports the single instance filter", async () => {
  captureURL();
  await api.logsSearch({ instances: [] });
  assert.equal(new URL(lastURL, "http://localhost").searchParams.has("instances"), false);

  captureURL();
  await api.logsSearch({ instance: "inst-a" });
  const url = new URL(lastURL, "http://localhost");
  assert.equal(url.searchParams.get("instance"), "inst-a");
  assert.equal(url.searchParams.has("instances"), false);
});

test("logsHistogram serializes instances the same way as logsSearch", async () => {
  captureURL();
  await api.logsHistogram({ instances: ["inst-a", "inst-b"] }, 60_000);
  const url = new URL(lastURL, "http://localhost");
  assert.equal(url.searchParams.get("instances"), "inst-a,inst-b");
  assert.equal(url.searchParams.get("bucket"), "60000");
});
