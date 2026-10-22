import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false,
  }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// Count constructions so tests can prove which transport the log viewer chose.
let eventSourceConstructions = 0;
class EventSourceStub {
  static instances: EventSourceStub[] = [];
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {
    eventSourceConstructions += 1;
    EventSourceStub.instances.push(this);
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });
Object.defineProperty(dom.window, "EventSource", { value: EventSourceStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");

const { api } = await import("../src/api.ts");
const { LogStream } = await import("../src/pages/InstanceLogStream.tsx");
const originalLogsSearch = api.logsSearch;

afterEach(() => {
  cleanup();
  api.logsSearch = originalLogsSearch;
  eventSourceConstructions = 0;
  EventSourceStub.instances = [];
});

const HOUR = 60 * 60 * 1000;

function renderStream(range: unknown, live = true) {
  return render(
    <I18nextProvider i18n={i18n}>
      <LogStream uuid="inst-log-1" live={live} follow={false} range={range as never} />
    </I18nextProvider>,
  );
}

test("a quick history range queries the persisted window instead of the live stream", async () => {
  const seen: Array<{ instance?: string; limit?: number; start?: string; end?: string; hide?: boolean }> = [];
  const before = Date.now();
  api.logsSearch = (async (f: { instance?: string; limit?: number; from?: number; to?: number }) => {
    seen.push({ instance: f.instance, limit: f.limit, start: f.from ? String(f.from) : undefined, end: f.to ? String(f.to) : undefined });
    return {
      items: [
        { id: 2, instance: "inst-log-1", node: "n", level: "error", source: "instance", time_ms: before - 1000, line: "CUDA out of memory" },
        { id: 1, instance: "inst-log-1", node: "n", level: "info", source: "instance", time_ms: before - 2000, line: "starting container" },
      ],
    };
  }) as typeof api.logsSearch;

  renderStream({ kind: "quick", key: "1h" });

  await waitFor(() => assert.ok(seen.length > 0, "history range must query the log store"));
  const call = seen[0];
  assert.equal(call.instance, "inst-log-1");
  assert.equal(call.limit, 2000, "the viewer reads a 2000-line tail");
  const start = Number(call.start);
  const end = Number(call.end);
  assert.ok(Math.abs(end - before) < 5_000, `end ${end} is not ~now`);
  assert.ok(Math.abs((end - start) - HOUR) < 5_000, `window ${end - start}ms is not ~1h`);

  // Newest-first store results render oldest-first, with the server arrival
  // timestamp prefixed so the history view is readable.
  await waitFor(() => {
    const body = document.body.textContent ?? "";
    assert.ok(body.includes("starting container"), `history lines missing: ${body.slice(0, 300)}`);
    assert.ok(body.indexOf("starting container") < body.indexOf("CUDA out of memory"), "history must read chronologically");
    assert.ok(/\[\d\d-\d\d \d\d:\d\d:\d\d\]/.test(body), `missing ts prefix in: ${body.slice(0, 300)}`);
  });
  assert.equal(eventSourceConstructions, 0, "history range must not open the live stream");
});

test("the live mode keeps the SSE stream, drops scrape lines, keeps engine text bare", async () => {
  api.logsSearch = (async () => {
    throw new Error("live mode must not query the store");
  }) as typeof api.logsSearch;
  renderStream(null, true);
  await waitFor(() => assert.equal(eventSourceConstructions, 1, "live mode must open the SSE stream"));
  assert.ok(EventSourceStub.instances[0].url.includes("/api/v1/instances/inst-log-1/logs"));
  const source = EventSourceStub.instances[0];
  source.onmessage?.({ data: "INFO engine ready" } as MessageEvent);
  await waitFor(() => assert.ok((document.body.textContent ?? "").includes("INFO engine ready")));
  const body = document.body.textContent ?? "";
  assert.ok(!/\[\d\d-\d\d \d\d:\d\d:\d\d\]/.test(body), "live engine text must not gain a ts prefix");
});

test("an empty history window says so instead of showing the live placeholder", async () => {
  api.logsSearch = (async () => ({ items: [] })) as typeof api.logsSearch;
  renderStream({ kind: "custom", startMS: Date.now() - 2 * HOUR, endMS: Date.now() - HOUR }, false);
  await waitFor(() => assert.ok(screen.getByText("No logs in the selected range")));
  assert.ok(screen.getByText(/persisted logs in the selected range/i), "history hint missing");
});

test("a truncated window warns about dropped lines instead of staying silent", async () => {
  // A realistic truncated:true response: exactly the requested LOG_TAIL_LIMIT
  // (2000) entries, not an empty page - the store only ever sets truncated
  // when it actually filled the requested limit.
  const entries = Array.from({ length: 2000 }, (_, i) => ({ id: 2000 - i, instance: "inst-log-1", node: "node-1", level: "", source: "instance", time_ms: 3000 - i, line: `line ${i}` }));
  api.logsSearch = (async () => ({ items: entries, truncated: true })) as typeof api.logsSearch;
  renderStream({ kind: "quick", key: "1h" });
  await waitFor(() => assert.ok(screen.getByText(/showing the newest 2000/i), "truncation warning missing"));
  assert.equal(screen.queryByText(/persisted logs in the selected range/i), null, "the truncation line replaces the history hint");
});
