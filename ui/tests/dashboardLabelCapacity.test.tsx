import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
// echarts needs a 2d context JSDOM cannot provide.
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
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
class ShimPort {
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  postMessage(data: unknown) { setTimeout(() => this.onmessage?.({ data }), 0); }
}
class ShimMessageChannel { port1 = new ShimPort(); port2 = new ShimPort(); }
Object.defineProperty(globalThis, "MessageChannel", { value: ShimMessageChannel, configurable: true });

const dashboard = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/dashboard.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      dashboard,
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "dashboard",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: DashboardPage } = await import("../src/pages/DashboardPage.tsx");
import type { CapacityGroup, DashboardSummary } from "../src/api";
import { list } from "./helpers/listPage.ts";

const group = (key: string, nodes: number, gpus: number, extra: Partial<CapacityGroup> = {}): CapacityGroup => ({
  key, nodes, gpus, vram_gib: gpus * 80, unusable_gpus: 0, ...extra,
});

// The fleet the server summarizes: n1 4 GPUs pool=a zone=x, n2 2 GPUs pool=b,
// n3 1 GPU unlabeled.
function summaryFor(labelKey?: string): DashboardSummary {
  const key = labelKey === "zone" ? "zone" : "pool";
  return {
    nodes: { total: 3, online: 2, offline: 1, connecting: 0, gpus: 7, unusable_gpus: 1, vram_gib: 560, memory_gib: 192 },
    deployments: { total: 5, running: 2, warming: 1, sleepable: 1 },
    by_vendor: [group("nvidia", 3, 7, { unusable_gpus: 1 })],
    label_keys: ["pool", "zone"],
    label_key: key,
    by_label: key === "pool"
      ? [group("pool=a", 1, 4), group("pool=b", 1, 2), group("", 1, 1, { unlabeled: true })]
      : [group("zone=x", 1, 4), group("", 2, 3, { unlabeled: true })],
  };
}

const originals = { health: api.health, nodes: api.nodes, deployments: api.deployments, dashboardSummary: api.dashboardSummary, providers: api.providers, metricsQuery: api.metricsQuery };
let summaryRequests: Array<string | undefined> = [];
function stubApi(summary: (labelKey?: string) => DashboardSummary | Promise<DashboardSummary>) {
  summaryRequests = [];
  api.health = async () => ({ status: "ok", sessions: 0 }) as never;
  // The dashboard must not page through the fleet to count it.
  api.nodes = async () => { throw new Error("the dashboard fetched the full node list"); };
  api.deployments = async () => { throw new Error("the dashboard fetched the full deployment list"); };
  api.dashboardSummary = async (labelKey?: string) => { summaryRequests.push(labelKey); return summary(labelKey); };
  api.providers = async () => list([]);
  api.metricsQuery = async () => list([]);
}
afterEach(() => {
  cleanup();
  Object.assign(api, originals);
});

function capacityCard() {
  const title = screen.getByText("Capacity by label");
  return within(title.closest(".ant-card") as HTMLElement);
}

function renderPage() {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>
        <DashboardPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("the dashboard shows the server's counts without fetching the node or deployment lists", async () => {
  stubApi(summaryFor);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(dashboard.stats.nodesTotal)));
  const stat = (title: string) => within(screen.getByText(title).closest(".ant-statistic") as HTMLElement);
  await waitFor(() => assert.ok(stat(dashboard.stats.nodesTotal).getByText("3")));
  assert.ok(stat(dashboard.stats.gpusTotal).getByText("7"));
  assert.ok(stat(dashboard.stats.deploymentsTotal).getByText("5"));
  assert.ok(stat(dashboard.stats.deploymentsRunning).getByText("2"));
  assert.ok(screen.getByText("Offline compute nodes: 1"));
  const vendor = within(screen.getByText("Capacity by vendor").closest(".ant-card") as HTMLElement);
  assert.ok(vendor.getByText("nvidia"));
  assert.ok(vendor.getByText("7 GPU · 560 GiB"));
  assert.deepEqual(summaryRequests, [undefined]);
});

test("the label capacity card groups by the server's key and asks for the one the user picks", async () => {
  stubApi(summaryFor);
  renderPage();
  await waitFor(() => assert.ok(capacityCard().getByText("pool=a")));
  const card = capacityCard();
  assert.ok(card.getByText("pool=b"));
  assert.ok(card.getByText("Not set"));
  assert.ok(card.getByText("4 GPU · 320 GiB"));

  const select = card.getByRole("combobox", { name: "Label key to group by" });
  fireEvent.mouseDown(select);
  let option: Element | null = null;
  await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="zone"]'))));
  fireEvent.click(option!);
  await waitFor(() => assert.ok(capacityCard().getByText("zone=x")));
  assert.equal(capacityCard().queryByText("pool=a"), null);
  assert.ok(capacityCard().getByText("3 GPU · 240 GiB"));
  assert.equal(summaryRequests.at(-1), "zone");
});

test("with nodes but no labels the card says how to get a grouping", async () => {
  stubApi(() => ({ ...summaryFor(), label_keys: [], label_key: "", by_label: [] }));
  renderPage();
  await waitFor(() => assert.ok(capacityCard().getByText(dashboard.capacity.noLabels)));
  assert.equal(capacityCard().queryByRole("combobox"), null);
});

test("with no nodes the card keeps the plain empty-capacity text", async () => {
  stubApi(() => ({
    nodes: { total: 0, online: 0, offline: 0, connecting: 0, gpus: 0, unusable_gpus: 0, vram_gib: 0, memory_gib: 0 },
    deployments: { total: 0, running: 0, warming: 0, sleepable: 0 },
    by_vendor: [], label_keys: [], label_key: "", by_label: [],
  }));
  renderPage();
  await waitFor(() => assert.equal(capacityCard().queryByText(dashboard.capacity.noLabels), null));
  await waitFor(() => assert.ok(capacityCard().getByText(dashboard.capacity.empty)));
});

test("a failed summary leaves the counts at zero instead of breaking the page", async () => {
  stubApi(() => Promise.reject(new Error("boom")));
  renderPage();
  await waitFor(() => assert.ok(summaryRequests.length > 0));
  await waitFor(() => assert.ok(capacityCard().getByText(dashboard.capacity.empty)));
});

// Current cards consume the inventory-filtered snapshot, never chart history.
for (const current of [undefined, { utilization_pct: 0, temperature_c: 41, power_watts: 123, memory_used_bytes: 12 * 1024 ** 3 }]) {
  test(`current cards use ${current ? "fresh values, including measured zero" : "an empty state without samples"} independently of historical curves`, async () => {
    stubApi(() => ({ ...summaryFor(), current_metrics: current }));
    const stamp = Date.now() - 5 * 60 * 60 * 1000;
    const points: [number, number][] = [[stamp, 93], [stamp + 60_000, 97]];
    api.metricsQuery = async () => list([{ labels: { node: "removed", gpu: "0" }, points }]);
    await act(async () => { renderPage(); });
    for (const [key, expected] of Object.entries({ averageUtilization: "0", maximumTemperature: "41", totalPower: "123", hostMemoryUsed: "12.0" })) {
      const card = screen.getByText(dashboard.stats[key]).closest(".ant-statistic")!;
      const value = card.querySelector(".ant-statistic-content-value")?.textContent ?? "";
      if (current) assert.equal(value, expected);
      else assert.doesNotMatch(value, /[0-9]/, "missing samples must not masquerade as zero");
    }
    const { getInstanceByDom } = await import("echarts/core");
    const plotted = [...document.querySelectorAll<HTMLElement>("[_echarts_instance_]")].flatMap((element) => {
      const option = getInstanceByDom(element)?.getOption() as { series?: { data?: unknown }[] } | undefined;
      return option?.series?.map((series) => series.data) ?? [];
    });
    assert.ok(plotted.some((data) => JSON.stringify(data) === JSON.stringify(points)), "history remains available after removal");
  });
}

for (const allFailed of [false, true]) {
  test(`${allFailed ? "all" : "one"} unavailable current metric preserves inventory and later inventory updates`, async () => {
    const current = {
      utilization_pct: allFailed ? null : 25,
      temperature_c: null,
      power_watts: allFailed ? null : 123,
      memory_used_bytes: allFailed ? null : 12 * 1024 ** 3,
    };
    let nodeCount = 3;
    stubApi((key) => {
      const summary = summaryFor(key);
      return { ...summary, nodes: { ...summary.nodes, total: nodeCount, online: nodeCount - 1 }, current_metrics: current };
    });
    await act(async () => { renderPage(); });
    const statValue = (key: string) => screen.getByText(dashboard.stats[key]).closest(".ant-statistic")!
      .querySelector(".ant-statistic-content-value")?.textContent;
    assert.equal(statValue("nodesTotal"), "3");
    assert.equal(statValue("gpusTotal"), "7");
    assert.equal(statValue("deploymentsTotal"), "5");
    assert.ok(capacityCard().getByText("pool=a"));
    for (const [key, value] of Object.entries({ averageUtilization: "25", maximumTemperature: null, totalPower: "123", hostMemoryUsed: "12.0" })) {
      if (allFailed || value === null) assert.doesNotMatch(statValue(key) ?? "", /[0-9]/);
      else assert.equal(statValue(key), value);
    }
    nodeCount = 4;
    fireEvent.mouseDown(capacityCard().getByRole("combobox", { name: "Label key to group by" }));
    let option: Element | null = null;
    await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="zone"]'))));
    fireEvent.click(option!);
    await waitFor(() => assert.equal(statValue("nodesTotal"), "4"));
    assert.ok(capacityCard().getByText("zone=x"));
    assert.equal(statValue("deploymentsTotal"), "5");
    assert.doesNotMatch(statValue("maximumTemperature") ?? "", /[0-9]/);
  });
}
