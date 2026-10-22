import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import { apiResponseError } from "../src/apiError.ts";
import { notifySessionAuthenticated, startSessionActivity, subscribeSessionChanges } from "../src/session.ts";

const dom = new JSDOM("", { url: "http://localhost/" });
Object.defineProperty(globalThis, "window", { value: dom.window, configurable: true });
Object.defineProperty(globalThis, "document", { value: dom.window.document, configurable: true });
const originalFetch = globalThis.fetch;
const originalNow = Date.now;
afterEach(() => {
  globalThis.fetch = originalFetch;
  Date.now = originalNow;
  notifySessionAuthenticated();
});

test("only management authentication failures expire the session, once across concurrent responses", async () => {
  const changes: string[] = [];
  const stop = subscribeSessionChanges((change) => changes.push(change));
  try {
    for (const [status, body] of [[401, { error: "upstream unauthorized" }], [500, { code: "AUTHENTICATION_REQUIRED" }], [401, { code: "INVALID_API_KEY" }]] as const) {
      await apiResponseError(new Response(JSON.stringify(body), { status }), "failed");
    }
    assert.deepEqual(changes, []);
    await Promise.all([1, 2, 3].map(() => apiResponseError(new Response('{"code":"AUTHENTICATION_REQUIRED"}', { status: 401 }), "failed")));
    assert.deepEqual(changes, ["expired"]);
  } finally { stop(); }
});

test("visible user activity keeps a long form alive once per minute without idle or background heartbeats", async () => {
  let now = 100_000;
  Date.now = () => now;
  let visibility = "visible";
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility });
  const requests: string[] = [];
  globalThis.fetch = async (url, init) => {
    requests.push(String(url));
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "include");
    return new Response(null, { status: 204 });
  };
  const stop = startSessionActivity();
  const activity = async (type: string) => {
    document.dispatchEvent(new dom.window.Event(type));
    await new Promise((resolve) => setTimeout(resolve, 0));
  };
  try {
    await activity("visibilitychange");
    assert.equal(requests.length, 0);
    await activity("scroll");
    assert.equal(requests.length, 0, "programmatic log scrolling must not count as operator activity");
    await activity("input");
    await activity("keydown");
    assert.equal(requests.length, 1);
    now += 60_000;
    await activity("input");
    assert.equal(requests.length, 2);
    now += 60_000;
    visibility = "hidden";
    await activity("wheel");
    assert.equal(requests.length, 2);
    visibility = "visible";
    await activity("visibilitychange");
    assert.equal(requests.length, 2);
    await activity("pointerdown");
    assert.equal(requests.length, 3);
    now += 60_000;
    await activity("wheel");
    assert.equal(requests.length, 4);
  } finally { stop(); }
});

test("activity network failure preserves the session and can retry on later activity", async () => {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  let now = 100_000;
  Date.now = () => now;
  let calls = 0;
  const changes: string[] = [];
  const unsubscribe = subscribeSessionChanges((change) => changes.push(change));
  globalThis.fetch = async () => { calls++; throw new Error("offline"); };
  const stop = startSessionActivity();
  try {
    document.dispatchEvent(new dom.window.Event("input"));
    await new Promise((resolve) => setTimeout(resolve, 0));
    now += 60_000;
    document.dispatchEvent(new dom.window.Event("input"));
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(calls, 2);
    assert.deepEqual(changes, []);
  } finally { stop(); unsubscribe(); }
});

test("other tabs share only session lifecycle events", () => {
  const changes: string[] = [];
  const stop = subscribeSessionChanges((change) => changes.push(change));
  try {
    window.dispatchEvent(new dom.window.StorageEvent("storage", { key: "gpuworks.session", newValue: JSON.stringify({ type: "expired", nonce: "other-tab" }) }));
    window.dispatchEvent(new dom.window.StorageEvent("storage", { key: "gpuworks.session", newValue: JSON.stringify({ type: "authenticated", nonce: "other-tab-2" }) }));
    assert.deepEqual(changes, ["expired", "authenticated"]);
  } finally { stop(); }
});
