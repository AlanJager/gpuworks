import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element", "Node", "MutationObserver"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { renderHook, act, cleanup, waitFor } = await import("@testing-library/react");
const { useMonitorPolling } = await import("../src/pages/monitor/useMonitorPolling.ts");
afterEach(cleanup);

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test("event bursts and enabling follow coalesce behind a pending request", async () => {
  const first = deferred<number>();
  const next = deferred<number>();
  let calls = 0;
  const fetcher = () => ++calls === 1 ? first.promise : next.promise;
  const { result, rerender } = renderHook(({ interval }) => useMonitorPolling(fetcher, interval, "1h"), { initialProps: { interval: 0 } });
  act(() => { void result.current.refresh(); void result.current.refresh(); });
  rerender({ interval: 10000 });
  assert.equal(calls, 1);
  assert.equal(result.current.loading, true);
  await act(async () => first.resolve(1));
  assert.equal(calls, 2);
  assert.equal(result.current.data, 1);
  assert.equal(result.current.loading, false);
  await act(async () => next.resolve(2));
  assert.equal(result.current.data, 2);
});

test("new queries hide old data and discard late old responses", async () => {
  const old = deferred<number>();
  const next = deferred<number>();
  const { result, rerender } = renderHook(({ query }) => useMonitorPolling(() => query === "1h" ? old.promise : next.promise, 0, query), { initialProps: { query: "1h" } });
  rerender({ query: "24h" });
  assert.equal(result.current.data, null);
  assert.equal(result.current.loading, true);
  await act(async () => next.resolve(24));
  await act(async () => old.resolve(1));
  assert.equal(result.current.data, 24);
});

test("a failed background refresh preserves data and reports the error", async () => {
  const next = deferred<number>();
  let calls = 0;
  const { result } = renderHook(() => useMonitorPolling(() => ++calls === 1 ? Promise.resolve(1) : next.promise, 0, "1h"));
  await waitFor(() => assert.equal(result.current.data, 1));
  act(() => { void result.current.refresh(); });
  assert.equal(result.current.loading, false);
  await act(async () => next.reject(new Error("offline")));
  assert.equal(result.current.data, 1);
  assert.equal(result.current.error?.message, "offline");
});

test("unmount cancels queued trailing refreshes", async () => {
  const pending = deferred<number>();
  let calls = 0;
  const { result, unmount } = renderHook(() => useMonitorPolling(() => { calls++; return pending.promise; }, 0, "1h"));
  act(() => { void result.current.refresh(); });
  unmount();
  await act(async () => pending.resolve(1));
  assert.equal(calls, 1);
});
