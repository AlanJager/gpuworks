import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// The service monitor page's PD card (effectiveness, fallback rate with its
// top reasons, prefill stage/queue, avg prompt tokens - each independently
// nullable) and the replica table's role badge - both only present on a PD
// deployment's monitorDeployment response (ADR 0028, item C of the PD web UI
// plan). Reuses monitorServiceMonitorPage.test.tsx's fixture and boilerplate;
// kept in its own file so the PD-specific assertions do not grow that one.
//
// Role-badge assertions are scoped to the replica table (screen.getByRole
// ("table"), the page's only <table>) rather than searched page-wide: the
// request-path StageFlow diagram always renders "Prefill"/"Decode" as stage
// labels, PD or not, so an unscoped query would collide with it.

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/monitor/d/102" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: ServiceMonitorPage } = await import("../src/pages/monitor/ServiceMonitorPage.tsx");
import type { MonitorDeployment } from "../src/api";
import { cursorPage } from "./helpers/listPage.ts";

const deployment = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-deployment.json", import.meta.url), "utf8")) as MonitorDeployment;
const originalMonitorDeployment = api.monitorDeployment;

const SAFE_LOGS_SEARCH: typeof api.logsSearch = async () => ({ items: [] });
const SAFE_INSTANCE_EVENTS: typeof api.instanceEvents = async () => cursorPage([]);
api.logsSearch = SAFE_LOGS_SEARCH;
api.instanceEvents = SAFE_INSTANCE_EVENTS;

afterEach(() => {
  cleanup();
  api.monitorDeployment = originalMonitorDeployment;
  api.logsSearch = SAFE_LOGS_SEARCH;
  api.instanceEvents = SAFE_INSTANCE_EVENTS;
});

function renderPage(entry = "/monitor/d/102") {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route path="/monitor/d/:id" element={<ServiceMonitorPage />} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("an ordinary (non-PD) deployment shows no PD card and no role in the replica table", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  assert.equal(screen.queryByText("PD disaggregation"), null);
  const table = screen.getByRole("table");
  assert.equal(within(table).queryByText("Decode"), null);
  assert.equal(within(table).queryByText("Prefill"), null);
});

test("the PD card renders every field's value, and the top fallback reasons", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    pd: {
      effectiveness_pct: 87.4,
      fallback_rate_pct: 3.2,
      fallbacks: { no_prefill_available: 5, prefill_5xx: 2, prefill_timeout: 1 },
      prefill_stage_avg_ms: 245,
      prefill_waiting_avg: 1.8,
      avg_prompt_tokens: 512,
      prefill_stage_p95_ms: 790,
      kv_transfer: { exposed: true, avg_ms: 1294.2, p95_ms: 1878, avg_mb: 217.1, mb_per_s: 167.7, failed: 2, expired: 0 },
    },
  });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("PD disaggregation")));
  const card = screen.getByTestId("pd-card");
  assert.ok(within(card).getByText("87.4%"));
  assert.ok(within(card).getByText("3.2%"));
  assert.ok(within(card).getByText("245 ms"));
  assert.ok(within(card).getByText("1.8"));
  assert.ok(within(card).getByText("512"));
  assert.ok(within(card).getByText("790 ms"));
  // The engine's own KV transfer, per transfer; a failure stands out.
  const kv = within(card).getByTestId("pd-kv-transfer");
  assert.ok(within(kv).getByText("1294 ms"));
  assert.ok(within(kv).getByText("1878 ms"));
  assert.ok(within(kv).getByText("217.1 MB"));
  assert.ok(within(kv).getByText("167.7 MB/s"));
  assert.ok(within(kv).getByText("2").closest(".ant-typography-danger"), "failures are flagged");
  assert.ok(within(kv).getByText("0"));
  // Its labels say KV, so none reads as the prefill stage's.
  for (const label of ["KV transfer mean", "KV transfer p95", "KV mean size", "KV transfer rate", "KV transfer failed", "KV transfer expired"]) {
    assert.ok(within(kv).getByText(label), label);
  }
  assert.ok(within(card).getByText("Top fallback reasons:"));
  // Sorted by count descending, and capped at the top three.
  assert.ok(within(card).getByText("no_prefill_available · 5"));
  assert.ok(within(card).getByText("prefill_5xx · 2"));
  assert.ok(within(card).getByText("prefill_timeout · 1"));
});

test("the PD card renders a dash for every null field, and hides the fallback reasons line when there are none", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    pd: {
      effectiveness_pct: null,
      fallback_rate_pct: null,
      fallbacks: {},
      prefill_stage_avg_ms: null,
      prefill_waiting_avg: null,
      avg_prompt_tokens: null,
      prefill_stage_p95_ms: null,
      kv_transfer: { exposed: true, avg_ms: null, p95_ms: null, avg_mb: null, mb_per_s: null, failed: null, expired: null },
    },
  });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("PD disaggregation")));
  const card = screen.getByTestId("pd-card");
  assert.equal(within(card).getAllByText("—").length, 12);
  assert.equal(within(card).queryByText("Top fallback reasons:"), null);
});

test("a PD mode whose engine exports no KV transfer metrics says so instead of dashes", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    pd: {
      effectiveness_pct: null, fallback_rate_pct: 0, fallbacks: {}, prefill_stage_avg_ms: 620, prefill_waiting_avg: 0,
      avg_prompt_tokens: 2048, prefill_stage_p95_ms: 900,
      kv_transfer: { exposed: false, avg_ms: null, p95_ms: null, avg_mb: null, mb_per_s: null, failed: null, expired: null },
    },
  });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("PD disaggregation")));
  const kv = within(screen.getByTestId("pd-card")).getByTestId("pd-kv-transfer");
  assert.equal(kv.textContent, "KV transferNot provided by the engine");
  assert.equal(within(kv).queryByText("KV transfer mean"), null);
  // The reason is one hover (or focus) away.
  await userEvent.setup().hover(within(kv).getByText("Not provided by the engine"));
  assert.ok(await screen.findByText(/this PD mode's KV connector exports no transfer metrics/));
});

test("the replica table shows a role badge per replica when the deployment is PD", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    replicas: [
      { ...deployment.replicas[0], role: "decode" },
      { ...deployment.replicas[1], role: "prefill" },
    ],
  });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("1111aaaa2222")));
  const table = screen.getByRole("table");
  assert.ok(within(table).getByText("Decode"));
  assert.ok(within(table).getByText("Prefill"));
});

test("the replica table shows no role badge - a dash - for an ordinary (non-PD) deployment's replicas", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("1111aaaa2222")));
  const table = screen.getByRole("table");
  assert.equal(within(table).queryByText("Decode"), null);
  assert.equal(within(table).queryByText("Prefill"), null);
});
