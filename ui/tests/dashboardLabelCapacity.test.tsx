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
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { dashboard, common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")) } }, defaultNS: "dashboard", interpolation: { escapeValue: false } });
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { analyticsApi } = await import("../src/analyticsApi.ts");
const { default: DashboardPage } = await import("../src/pages/DashboardPage.tsx");
import type { DashboardSummary } from "../src/api";

const summary: DashboardSummary = {
  nodes: { total: 3, online: 2, offline: 1, connecting: 0, gpus: 7, unusable_gpus: 1, vram_gib: 560, memory_gib: 192 },
  deployments: { total: 5, running: 2, warming: 1, sleepable: 1 },
  by_vendor: [{ key: "nvidia", nodes: 3, gpus: 7, vram_gib: 560, unusable_gpus: 1 }],
  label_keys: ["pool", "zone"], label_key: "pool",
  by_label: [{ key: "pool=a", nodes: 1, gpus: 4, vram_gib: 320, unusable_gpus: 0 }],
};
const originals = {
  health: api.health, dashboardSummary: api.dashboardSummary, providers: api.providers,
  modelsPage: api.modelsPage, gpus: api.gpus,
  nodesPage: api.nodesPage, monitorOverview: api.monitorOverview, storages: api.storages,
  storageUsage: api.storageUsage, storage: api.storage, usage: analyticsApi.usage,
};
let requestedLabels: Array<string | undefined> = [];
function stub() {
  requestedLabels = [];
  api.health = async () => ({ status: "ok", sessions: 0 }) as never;
  api.dashboardSummary = async (key?: string) => { requestedLabels.push(key); return { ...summary, label_key: key ?? "pool", by_label: key === "zone" ? [{ key: "zone=x", nodes: 2, gpus: 6, vram_gib: 480, unusable_gpus: 0 }] : summary.by_label }; };
  api.providers = async () => ({ items: [], total: 0 });
  api.modelsPage = async () => ({ items: [], total: 0, summary: { registered: 0, ready_files: 0, sources: 0, weight_gib: 0 } }) as never;
  api.gpus = async () => ({ items: [{ uuid: "gpu-1", name: "H100", arch_family: "Hopper", vendor: "nvidia", memory_total_bytes: 80 * 1024 ** 3, allocation_state: "available" }], overview: { total: 1, allocation: { allocatable: 1, unusable: 0 } } }) as never;
  api.nodesPage = async () => ({ items: [], total: 0 }) as never;
  api.monitorOverview = async () => ({ as_of: 1000, stale: false, services: [], signals: [
    { key: "ready_replicas", collected: true, value: 2, denominator: 3 },
    { key: "request_rate", collected: true, value: 12.3, denominator: 19.96 },
    { key: "error_rate", collected: true, value: 1.2 },
    { key: "ttft", collected: true, value: 345.6, detail: { upper_bound: true } },
    { key: "waiting", collected: true, value: 2.4 },
  ] }) as never;
  api.storages = async () => ({ items: [], total: 0 });
  api.storageUsage = async () => ({ items: [], total: 0 });
  api.storage = async () => { throw Error("no storages"); };
  analyticsApi.usage = async () => ({ range: { from_ms: 1000, to_ms: 2000, granularity: "hour", time_zone: "UTC", earliest_ms: 1000, live_since_ms: 1000, buckets: [] }, totals: { requests: 8, no_usage: 1, prompt_tokens: 135, completion_tokens: 42, reasoning_tokens: 0, reasoning_reported: 0, cached_tokens: 32, cache_reported: 1, cache_reported_prompt: 135 }, rows: [], series: [] });
}
afterEach(() => { cleanup(); Object.assign(api, originals); analyticsApi.usage = originals.usage; });
function renderPage() { render(<I18nextProvider i18n={i18n}><MemoryRouter><DashboardPage /></MemoryRouter></I18nextProvider>); }

test("platform cards show fleet aggregates with one top-right module entry each", async () => {
  stub(); renderPage();
  assert.equal(document.querySelector(".page-header-title")?.textContent, "Overview");
  assert.ok(document.querySelector(".page-header")?.textContent?.includes("Platform-wide compute"));
  const cards = document.querySelectorAll(".dashboard-overview-resource");
  await waitFor(() => assert.equal(cards[0].querySelector(".dashboard-overview-resource-value")?.textContent, "3nodes"));
  assert.equal(document.querySelectorAll(".dashboard-overview-resource.ant-card").length, 4);
  assert.equal(document.querySelectorAll(".dashboard-overview-panel.ant-card").length, 6);
  assert.deepEqual([...cards].map((card) => card.querySelector<HTMLAnchorElement>("a.dashboard-overview-module-arrow")?.getAttribute("href")), ["/nodes", "/gpus", "/deployments", undefined]);
  assert.ok([...cards].slice(0, 3).every((card) => card.querySelectorAll("a").length === 1));
  assert.equal(cards[3].querySelectorAll("a").length, 0);
  assert.equal(cards[3].querySelectorAll("button.dashboard-overview-module-arrow").length, 1);
  assert.deepEqual([...cards[0].querySelectorAll(".dashboard-overview-resource-facts span")].map((status) => status.textContent), ["Online 2", "Offline 1"]);
  assert.deepEqual([...cards[0].querySelectorAll(".dashboard-overview-resource-facts span")].map((status) => [...status.classList].find((name) => name.startsWith("dashboard-overview-status-") && name !== "dashboard-overview-status")), ["dashboard-overview-status-success", "dashboard-overview-status-danger"]);
  assert.deepEqual([...cards[1].querySelectorAll(".dashboard-overview-resource-facts span")].map((status) => status.textContent), ["Allocatable 1"]);
  assert.deepEqual([...cards[2].querySelectorAll(".dashboard-overview-resource-facts span")].map((status) => status.textContent), ["Running 2", "Starting 1", "Other 2"]);
  assert.ok(cards[2].querySelector(".dashboard-overview-status-neutral")?.textContent?.includes("Other 2"));
  assert.equal(cards[2].querySelector(".dashboard-overview-resource-detail")?.textContent, "Sleepable 1");
  assert.equal(cards[3].querySelectorAll(".dashboard-overview-status").length, 0);
  assert.ok([...cards].every((card) => !card.querySelector(".dashboard-overview-resource-foot")));
  assert.equal(cards[2].querySelector(".dashboard-overview-resource-title")?.textContent, "Deployments");
  assert.ok(cards[2].querySelector(".dashboard-overview-resource-value")?.textContent?.startsWith("5services"));
  assert.equal(cards[1].querySelector(".dashboard-overview-resource-value")?.textContent, "1cards");
  assert.deepEqual([...document.querySelectorAll(".dashboard-overview-inference > .dashboard-overview-panel h2")].map((title) => title.textContent), ["Inference usage", "Monitor summary"]);
  await waitFor(() => assert.ok(screen.getByText("H100")));
  assert.equal(document.querySelectorAll(".dashboard-overview-composition-grid > .dashboard-overview-distribution").length, 2);
  assert.deepEqual(requestedLabels, [undefined]);
});

test("node connection states keep their names and counts alongside green, amber and red indicators", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 5, connecting: 2 } });
  renderPage();
  const node = document.querySelector(".dashboard-overview-resource")!;
  await waitFor(() => assert.ok(node.textContent?.includes("Connecting 2")));
  assert.deepEqual([...node.querySelectorAll(".dashboard-overview-resource-facts span")].map((status) => [status.textContent, [...status.classList].find((name) => name.startsWith("dashboard-overview-status-"))]), [
    ["Online 2", "dashboard-overview-status-success"],
    ["Connecting 2", "dashboard-overview-status-warning"],
    ["Offline 1", "dashboard-overview-status-danger"],
  ]);
});

test("overview token total adds reported input and output once and opens the same Usage Analytics range", async () => {
  stub();
  const usage = await analyticsApi.usage({ from: 1000, to: 2000, granularity: "hour" });
  analyticsApi.usage = async () => ({ ...usage, totals: { ...usage.totals, prompt_tokens: 1_000_000, completion_tokens: 234_567, reasoning_tokens: 11, reasoning_reported: 1 } });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("1M")));
  const metrics = [...document.querySelectorAll<HTMLElement>(".dashboard-overview-usage .dashboard-overview-metric")];
  assert.deepEqual(metrics.map((metric) => metric.querySelector("strong")?.textContent), ["1.23M", "1M", "234.57K"]);
  assert.deepEqual(metrics.map((metric) => metric.querySelector("strong")?.title), ["1,234,567", "1,000,000", "234,567"]);
  assert.ok(metrics[0].textContent?.includes("Requests without usage: 1"));
  assert.deepEqual(metrics.map((metric) => metric.querySelector<HTMLElement>(".dashboard-overview-metric-tip")?.getAttribute("tabindex")), ["0", "0", "0"]);
  assert.deepEqual(metrics.map((metric) => metric.querySelector<HTMLElement>(".dashboard-overview-metric-tip")?.getAttribute("aria-label")), [
    "Reported input plus output tokens; requests without usage are excluded.",
    "Reported input tokens, including cached tokens.",
    "Reported output tokens; reported reasoning tokens are part of this count.",
  ]);
  assert.ok(!document.querySelector(".dashboard-overview-usage")?.textContent?.includes("Cached tokens"));
  assert.ok(metrics.every((metric) => metric.tagName === "DIV"));
  assert.deepEqual([...document.querySelectorAll<HTMLAnchorElement>(".dashboard-overview-usage a")].map((link) => link.getAttribute("href")), ["/analytics/usage?range=custom&from=1000&to=2000&gran=hour"]);
});

test("monitor summary uses current monitor signals and links to the monitor's matching range", async () => {
  stub(); renderPage();
  const monitor = document.querySelector(".dashboard-overview-monitor")!;
  await waitFor(() => assert.deepEqual([...monitor.querySelectorAll(".dashboard-overview-metric strong")].map((value) => value.textContent), ["12.3 req/s · 20 conn", "1.2%", "345.6 ms", "2.4"]));
  assert.equal(monitor.querySelector(".dashboard-overview-replica-count")?.textContent, "Ready replicas 2/3");
  assert.equal(monitor.querySelector(".dashboard-overview-count")?.textContent, "Last 5 minutes");
  assert.equal(monitor.querySelector("a.dashboard-overview-module-arrow")?.getAttribute("href"), "/monitor?range=6h");
  assert.equal(monitor.querySelectorAll(".dashboard-overview-metric-tip").length, 4);
  cleanup();
  api.monitorOverview = async () => ({ stale: false, services: [], signals: [
    { key: "ready_replicas", collected: true, value: 0, denominator: 0 },
    { key: "request_rate", collected: true, value: 0, detail: { no_recent_requests: true } },
    { key: "error_rate", collected: false },
    { key: "ttft", collected: true, value: null, detail: { no_recent_requests: true } },
    { key: "waiting", collected: false },
  ] }) as never;
  renderPage();
  const idle = document.querySelector(".dashboard-overview-monitor")!;
  await waitFor(() => assert.equal(idle.querySelector(".dashboard-overview-count")?.textContent, "No gateway requests in 5 minutes"));
  assert.equal(idle.querySelector(".dashboard-overview-replica-count")?.textContent, "Ready replicas 0/0");
  assert.deepEqual([...idle.querySelectorAll(".dashboard-overview-metric strong")].map((value) => value.textContent), ["0.0 req/s", "—", "—", "—"]);
  cleanup();
  api.monitorOverview = async () => ({ stale: false, services: [], signals: [] }) as never;
  renderPage();
  await waitFor(() => assert.equal(document.querySelector(".dashboard-overview-replica-count")?.textContent, "Ready replicas —/—"));
});

test("GPU usage card shows fresh dashboard samples and opens the GPU analytics tab as one link", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, current_metrics: { utilization_pct: 37.5, temperature_c: 81, power_watts: 1234, memory_used_bytes: null } });
  renderPage();
  const link = await screen.findByRole("link", { name: "View GPU utilization in Usage Analytics" });
  assert.equal(link.getAttribute("href"), "/analytics/gpus");
  assert.equal(link.querySelectorAll("a").length, 0);
  const capacityRow = document.querySelector(".dashboard-overview-capacity")!;
  assert.equal(capacityRow.firstElementChild, link);
  assert.ok(capacityRow.lastElementChild?.textContent?.includes("Shared storage used / total"));
  assert.ok(link.textContent?.includes("Current samples · last 2 minutes"));
  assert.deepEqual([...link.querySelectorAll(".dashboard-overview-metric strong")].map((value) => value.textContent), ["37.5%", "81 °C", "1.23 kW"]);
  cleanup();
  api.dashboardSummary = async () => ({ ...summary, current_metrics: { utilization_pct: 0, temperature_c: null, power_watts: 0, memory_used_bytes: null } });
  renderPage();
  const values = [...(await screen.findByRole("link", { name: "View GPU utilization in Usage Analytics" })).querySelectorAll(".dashboard-overview-metric strong")].map((value) => value.textContent);
  assert.deepEqual(values, ["0.0%", "—", "0 W"]);
});

test("platform posture shows management health and active sessions without duplicating hardware or deployment metrics", async () => {
  stub();
  api.health = async () => ({ status: "ok", version: "v1.2.3", sessions: 3 }) as never;
  api.dashboardSummary = async () => ({ ...summary, current_metrics: {
    utilization_pct: 0,
    temperature_c: 37,
    power_watts: 64,
    memory_used_bytes: 14.8 * 1024 ** 3,
  } });
  renderPage();
  const posture = document.querySelector(".dashboard-overview-posture")!;
  await waitFor(() => assert.ok(posture.textContent?.includes("Active agent sessions: 3")));
  assert.ok(posture.textContent?.includes("Platform status"));
  assert.ok(posture.textContent?.includes("Platform version v1.2.3"));
  assert.ok(posture.textContent?.includes("Management service Normal"));
  assert.ok(!posture.textContent?.includes("Sleepable"));
  assert.ok(!posture.textContent?.includes("14.8"));
  assert.ok(!posture.textContent?.includes("37"));
  assert.equal(posture.querySelectorAll("a").length, 0);
});

test("platform posture does not report normal service or zero sessions when health is unavailable", async () => {
  stub();
  api.health = async () => { throw new Error("health unavailable"); };
  renderPage();
  const posture = document.querySelector(".dashboard-overview-posture")!;
  await waitFor(() => assert.ok(posture.textContent?.includes("Active agent sessions: —")));
  assert.ok(posture.textContent?.includes("Management service —"));
  assert.ok(posture.textContent?.includes("Platform version —"));
  assert.ok(!posture.textContent?.includes("Normal"));
});

test("GPU distributions show total VRAM by vendor, model and node label instead of card percentages", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary,
    nodes: { ...summary.nodes, gpus: 3, vram_gib: 120 },
    by_vendor: [{ key: "nvidia", nodes: 2, gpus: 3, vram_gib: 120, unusable_gpus: 0 }],
    by_label: [
      { key: "pool=a", nodes: 1, gpus: 2, vram_gib: 72, unusable_gpus: 0 },
      { key: "", unlabeled: true, nodes: 1, gpus: 1, vram_gib: 48, unusable_gpus: 0 },
    ],
  });
  api.gpus = async () => ({ items: [
    { uuid: "a", name: "GeForce RTX 4090", vendor: "nvidia", memory_total_bytes: 48 * 1024 ** 3, allocation_state: "available" },
    { uuid: "b", name: "GeForce RTX 4090", vendor: "nvidia", memory_total_bytes: 48 * 1024 ** 3, allocation_state: "available" },
    { uuid: "c", name: "GeForce RTX 3090", vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "available" },
  ], overview: { total: 3 } }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("GeForce RTX 4090")));
  assert.ok(screen.getByText("GPU capacity distribution"));
  assert.ok(screen.getByText("GPU capacity by node label"));
  const panels = document.querySelectorAll(".dashboard-overview-composition > .dashboard-overview-panel");
  const vendor = panels[0].querySelector(".dashboard-overview-distribution-row")!;
  const models = panels[0].querySelectorAll(".dashboard-overview-distribution:nth-child(2) .dashboard-overview-distribution-row");
  const labels = panels[1].querySelectorAll(".dashboard-overview-distribution-row");
  assert.equal(vendor.querySelectorAll("span")[1]?.textContent, "120.0 GiB");
  assert.deepEqual([...models].map((row) => row.querySelectorAll("span")[1]?.textContent), ["96.0 GiB", "24.0 GiB"]);
  assert.deepEqual([...labels].map((row) => row.querySelectorAll("span")[1]?.textContent), ["72.0 GiB", "48.0 GiB"]);
  assert.equal((models[0].querySelector("i b") as HTMLElement).style.width, "80%");
  assert.equal((labels[0].querySelector("i b") as HTMLElement).style.width, "60%");
  assert.ok([...panels].every((panel) => !panel.textContent?.includes("%")));
});

test("mixed vendors and models retain every capacity group and each GPU allocation state", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary,
    nodes: { ...summary.nodes, total: 4, online: 2, connecting: 1, offline: 1, gpus: 4, vram_gib: 200 },
    by_vendor: [
      { key: "nvidia", nodes: 2, gpus: 2, vram_gib: 120, unusable_gpus: 0 },
      { key: "ascend", nodes: 1, gpus: 1, vram_gib: 64, unusable_gpus: 1 },
      { key: "amd", nodes: 1, gpus: 1, vram_gib: 16, unusable_gpus: 0 },
    ],
    by_label: [
      { key: "pool=production", nodes: 2, gpus: 2, vram_gib: 144, unusable_gpus: 0 },
      { key: "pool=development", nodes: 2, gpus: 2, vram_gib: 56, unusable_gpus: 1 },
    ],
  });
  api.gpus = async () => ({ items: [
    { uuid: "n1", name: "H100", vendor: "nvidia", memory_total_bytes: 80 * 1024 ** 3, allocation_state: "available" },
    { uuid: "n2", name: "L40", vendor: "nvidia", memory_total_bytes: 40 * 1024 ** 3, allocation_state: "allocated" },
    { uuid: "a1", name: "Ascend 910B", vendor: "ascend", memory_total_bytes: 64 * 1024 ** 3, allocation_state: "unusable" },
    { uuid: "m1", name: "MI210", vendor: "amd", memory_total_bytes: 16 * 1024 ** 3, allocation_state: "unknown" },
  ], overview: { total: 4, allocation: { allocated: 1, allocatable: 1, unusable: 1, unknown: 1 } } }) as never;
  renderPage();
  await waitFor(() => assert.ok(document.querySelector(".dashboard-overview-composition-grid")?.textContent?.includes("Ascend 910B")));
  const vendorRows = document.querySelectorAll(".dashboard-overview-composition-grid > .dashboard-overview-distribution:first-child .dashboard-overview-distribution-row");
  const modelRows = document.querySelectorAll(".dashboard-overview-composition-grid > .dashboard-overview-distribution:nth-child(2) .dashboard-overview-distribution-row");
  const labelRows = document.querySelectorAll(".dashboard-overview-composition .dashboard-overview-panel:nth-child(2) .dashboard-overview-distribution-row");
  assert.deepEqual([...vendorRows].map((row) => row.textContent), ["nvidia120.0 GiB", "ascend64.0 GiB", "amd16.0 GiB"]);
  assert.deepEqual([...modelRows].map((row) => row.querySelectorAll("span")[1]?.textContent), ["80.0 GiB", "64.0 GiB", "40.0 GiB", "16.0 GiB"]);
  assert.deepEqual([...labelRows].map((row) => row.querySelectorAll("span")[1]?.textContent), ["144.0 GiB", "56.0 GiB"]);
  const gpu = document.querySelectorAll(".dashboard-overview-resource")[1];
  assert.deepEqual([...gpu.querySelectorAll(".dashboard-overview-resource-facts span")].map((status) => status.textContent), ["Allocatable 1", "Allocated 1", "Unavailable 1", "Unknown 1"]);
});

test("eight model and label groups fit without pagination", async () => {
  stub();
  const capacities = Array.from({ length: 8 }, (_, index) => 80 - index * 10);
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, gpus: 8, vram_gib: 360 }, by_label: capacities.map((capacity, index) => ({ key: `pool=value-${index}`, nodes: 1, gpus: 1, vram_gib: capacity, unusable_gpus: 0 })) });
  api.gpus = async () => ({ items: capacities.map((capacity, index) => ({ uuid: `gpu-${index}`, name: `model-${index}`, vendor: "nvidia", memory_total_bytes: capacity * 1024 ** 3, allocation_state: "available" })), overview: { total: 8 } }) as never;
  renderPage();
  await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-composition-grid > .dashboard-overview-distribution:nth-child(2) .dashboard-overview-distribution-row").length, 8));
  assert.equal(document.querySelectorAll(".dashboard-overview-composition .dashboard-overview-panel:nth-child(2) .dashboard-overview-distribution-row").length, 8);
  assert.equal(document.querySelectorAll(".dashboard-overview-composition .ant-pagination").length, 0);
});

test("the ninth model and label groups page independently without changing full capacity totals", async () => {
  stub();
  const capacities = Array.from({ length: 9 }, (_, index) => 90 - index * 10);
  api.dashboardSummary = async (key?: string) => ({ ...summary,
    nodes: { ...summary.nodes, gpus: 9, vram_gib: 450 },
    label_key: key ?? "pool",
    by_label: key === "zone"
      ? [{ key: "zone=east", nodes: 1, gpus: 1, vram_gib: 450, unusable_gpus: 0 }]
      : capacities.map((capacity, index) => ({ key: `pool=value-${index}`, nodes: 1, gpus: 1, vram_gib: capacity, unusable_gpus: 0 })),
  });
  api.gpus = async () => ({ items: capacities.map((capacity, index) => ({ uuid: `gpu-${index}`, name: `model-${index}`, vendor: "nvidia", memory_total_bytes: capacity * 1024 ** 3, allocation_state: "available" })), overview: { total: 9 } }) as never;
  renderPage();
  const modelList = document.querySelector(".dashboard-overview-composition-grid > .dashboard-overview-distribution:nth-child(2)")!;
  const labelList = () => document.querySelector(".dashboard-overview-composition .dashboard-overview-panel:nth-child(2) .dashboard-overview-distribution")!;
  await waitFor(() => assert.equal(modelList.querySelectorAll(".dashboard-overview-distribution-row").length, 8));
  await waitFor(() => assert.equal(labelList().querySelectorAll(".dashboard-overview-distribution-row").length, 8));
  assert.equal((modelList.querySelector(".dashboard-overview-distribution-row i b") as HTMLElement).style.width, `${90 / 450 * 100}%`);
  assert.equal((labelList().querySelector(".dashboard-overview-distribution-row i b") as HTMLElement).style.width, `${90 / 450 * 100}%`);
  fireEvent.click(modelList.querySelector(".ant-pagination-item-2")!);
  await waitFor(() => assert.equal(modelList.querySelectorAll(".dashboard-overview-distribution-row").length, 1));
  assert.ok(modelList.textContent?.includes("model-8") && !modelList.textContent?.includes("model-0"));
  assert.ok(labelList().textContent?.includes("pool:value-0"));
  fireEvent.click(labelList().querySelector(".ant-pagination-item-2")!);
  await waitFor(() => assert.ok(labelList().textContent?.includes("pool:value-8") && !labelList().textContent?.includes("pool:value-0")));
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Label key to group by" }));
  let option: Element | null = null;
  await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="zone"]'))));
  fireEvent.click(option!);
  await waitFor(() => assert.ok(screen.getByText("zone:east")));
  assert.equal(document.querySelectorAll(".dashboard-overview-composition .dashboard-overview-panel:nth-child(2) .dashboard-overview-distribution-row").length, 1);
  assert.equal(document.querySelectorAll(".dashboard-overview-composition .dashboard-overview-panel:nth-child(2) .ant-pagination").length, 0);
  assert.ok(modelList.textContent?.includes("model-8"));
});

test("a model with missing GPU memory does not present a partial total as full capacity", async () => {
  stub();
  api.gpus = async () => ({ items: [
    { uuid: "a", name: "H100", vendor: "nvidia", memory_total_bytes: 80 * 1024 ** 3, allocation_state: "available" },
    { uuid: "b", name: "H100", vendor: "nvidia", memory_total_bytes: 0, allocation_state: "available" },
  ], overview: { total: 2 } }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("H100")));
  const model = document.querySelector(".dashboard-overview-composition-grid > .dashboard-overview-distribution:nth-child(2) .dashboard-overview-distribution-row")!;
  assert.equal(model.querySelectorAll("span")[1]?.textContent, "—");
  assert.equal(model.querySelector("i"), null);
});

test("capacity distribution excludes cards owned elsewhere while GPU inventory still counts them", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, gpus: 1, vram_gib: 80 }, by_vendor: [{ key: "nvidia", nodes: 1, gpus: 1, vram_gib: 80, unusable_gpus: 0 }] });
  api.gpus = async () => ({ items: [
    { uuid: "owned", name: "H100", vendor: "nvidia", memory_total_bytes: 80 * 1024 ** 3, allocation_state: "available" },
    { uuid: "passed", name: "passed-through card", vendor: "amd", memory_total_bytes: 64 * 1024 ** 3, ownership: "passthrough", allocation_state: "unusable" },
  ], overview: { total: 2 } }) as never;
  renderPage();
  await waitFor(() => assert.ok(document.querySelector(".dashboard-overview-composition-grid")?.textContent?.includes("H100")));
  const distribution = document.querySelector(".dashboard-overview-composition-grid")!;
  assert.ok(!distribution.textContent?.includes("passed-through card"));
  assert.equal(document.querySelectorAll(".dashboard-overview-resource")[1].querySelector(".dashboard-overview-resource-value")?.textContent, "2cards");
});

test("cloud card arrow offers both module destinations", async () => {
  stub(); renderPage();
  const cloud = document.querySelectorAll(".dashboard-overview-resource")[3];
  fireEvent.click(cloud.querySelector("button.dashboard-overview-module-arrow")!);
  await waitFor(() => assert.ok(screen.getByRole("link", { name: "Public clouds" })));
  assert.equal(screen.getByRole("link", { name: "Public clouds" }).getAttribute("href"), "/providers");
  assert.equal(screen.getByRole("link", { name: "Private clouds" }).getAttribute("href"), "/zstack");
});

test("monitor replica summary uses current replica counts rather than model file counts", async () => {
  stub();
  api.modelsPage = async () => ({ items: [
    { id: 1, name: "ready with one bad replica", presence: { ready_nodes: 1, ready_storages: 0, failed: 1, in_flight: 0 } },
    { id: 2, name: "not ready", presence: { ready_nodes: 0, ready_storages: 0, failed: 1, in_flight: 0 } },
  ], total: 2 }) as never;
  renderPage();
  const monitor = document.querySelector(".dashboard-overview-monitor")!;
  await waitFor(() => assert.equal(monitor.querySelector(".dashboard-overview-replica-count")?.textContent, "Ready replicas 2/3"));
  assert.equal(document.querySelectorAll(".dashboard-overview-resource").length, 4);
  assert.equal(monitor.querySelector("a")?.getAttribute("href"), "/monitor?range=6h");
});

test("label selection requests the server grouping for that key", async () => {
  stub(); renderPage();
  await waitFor(() => assert.ok(screen.getByText("pool:a")));
  assert.ok(document.querySelector(".dashboard-overview-panel-head .dashboard-overview-label-key.ant-select-borderless"));
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Label key to group by" }));
  let option: Element | null = null;
  await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="zone"]'))));
  fireEvent.click(option!);
  await waitFor(() => assert.ok(screen.getByText("zone:x")));
  assert.equal(requestedLabels.at(-1), "zone");
});

test("no label keys leave the label panel without a meaningless selector", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, label_keys: [], label_key: "", by_label: [] });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("GPU nodes have no labels")));
  assert.equal(document.querySelectorAll(".dashboard-overview-label-key").length, 0);
});

test("attention stays below the page header and keeps direct links when more issues collapse", async () => {
  stub();
  api.nodesPage = async () => ({ items: [{ name: "host3", hostname: "host3" }], total: 1 }) as never;
  api.gpus = async () => ({ items: [{ uuid: "gpu-1", name: "H100", arch_family: "Hopper", vendor: "nvidia", allocation_state: "unusable" }], overview: { total: 1, allocation: { allocatable: 0, unusable: 1 } } }) as never;
  api.monitorOverview = async () => ({ services: [{ deployment_id: 7, name: "service7", state: "running", health: { status: "failed" } }] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("button", { name: "1 more" })));
  const strip = document.querySelector(".dashboard-overview-attention");
  assert.ok(strip);
  assert.ok(strip.classList.contains("ant-alert-warning"));
  // The shared page header's root is .page-header (page-header-wrap went with the old header).
  assert.ok(strip.previousElementSibling?.classList.contains("page-header"));
  assert.deepEqual([...strip.querySelectorAll<HTMLAnchorElement>("a")].map((link) => link.getAttribute("href")), ["/nodes", "/gpus?gpu=gpu-1"]);
  assert.equal(document.querySelectorAll(".dashboard-overview-issues").length, 0);
  fireEvent.click(screen.getByRole("button", { name: "1 more" }));
  await waitFor(() => assert.equal(screen.getByRole("link", { name: /service7/ }).getAttribute("href"), "/monitor/d/7"));
});

test("an offline node's unknown GPU is visible in inventory state but not counted again as a confirmed GPU fault", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 3, online: 2, offline: 1, unusable_gpus: 2 } });
  api.nodesPage = async () => ({ items: [{ name: "host3", hostname: "host3" }], total: 1 }) as never;
  api.gpus = async () => ({ items: [
    { uuid: "gpu-offline", name: "RTX 4090", node_state: "offline", allocation_state: "unknown" },
    { uuid: "gpu-online", name: "RTX 4090", node_state: "online", allocation_state: "partial" },
    { uuid: "gpu-fault", name: "RTX 3090", node_state: "online", allocation_state: "unusable" },
  ], overview: { total: 3, allocation: { allocatable: 1, unusable: 1, unknown: 1 } } }) as never;
  renderPage();
  await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 2));
  const attention = document.querySelector(".dashboard-overview-attention")!;
  assert.ok(attention.textContent?.includes("Needs attention 2"));
  assert.ok(attention.textContent?.includes("host3") && attention.textContent?.includes("RTX 3090"));
  assert.ok(!attention.textContent?.includes("RTX 4090"));
  assert.ok(document.querySelectorAll(".dashboard-overview-resource")[1].textContent?.includes("Unknown 1"));
});

test("five issues retain object links while six switch to a single category count", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 2, online: 2, offline: 0 } });
  api.gpus = async () => ({ items: Array.from({ length: 5 }, (_, index) => ({ uuid: `gpu-${index}`, name: `card-${index}`, vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "unusable" })), overview: { total: 5, allocation: { unusable: 5 } } }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("button", { name: "3 more" })));
  assert.ok(document.querySelector(".dashboard-overview-attention")?.textContent?.includes("Needs attention 5"));
  assert.ok(screen.getByRole("link", { name: /card-0/ }).getAttribute("href")?.includes("gpu-0"));
  cleanup();
  api.gpus = async () => ({ items: Array.from({ length: 6 }, (_, index) => ({ uuid: `gpu-${index}`, name: `card-${index}`, vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "unusable" })), overview: { total: 6, allocation: { unusable: 6 } } }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("link", { name: "Unavailable GPUs 6" })));
  assert.ok(document.querySelector(".dashboard-overview-attention")?.textContent?.includes("Needs attention 1 category"));
  assert.equal(document.querySelectorAll(".dashboard-overview-attention-link").length, 1);
  assert.ok(!document.querySelector(".dashboard-overview-attention")?.textContent?.includes("card-0"));
});

test("attention shows every item that fits and folds only the remainder after resizing", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 5, online: 0, offline: 5 } });
  api.nodesPage = async () => ({ items: Array.from({ length: 5 }, (_, index) => ({ name: `node-${index + 1}`, hostname: `node-${index + 1}` })), total: 5 }) as never;
  let availableWidth = 900;
  const clientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");
  const offsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get() { return this.classList.contains("dashboard-overview-attention-content") ? availableWidth : 0; } });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get() {
    if (this.classList.contains("dashboard-overview-attention-heading")) return 100;
    if (this.classList.contains("dashboard-overview-attention-measure-entry")) return 120;
    if (this.classList.contains("dashboard-overview-attention-measure-more")) return 70;
    return 0;
  } });
  try {
    renderPage();
    await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 5));
    assert.ok(!screen.queryByRole("button", { name: /more/ }));
    availableWidth = 450;
    fireEvent.resize(window);
    await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 2));
    assert.ok(screen.getByRole("button", { name: "3 more" }));
    availableWidth = 580;
    fireEvent.resize(window);
    await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 3));
    assert.ok(screen.getByRole("button", { name: "2 more" }));
  } finally {
    if (clientWidth) Object.defineProperty(HTMLElement.prototype, "clientWidth", clientWidth);
    else delete (HTMLElement.prototype as unknown as { clientWidth?: number }).clientWidth;
    if (offsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidth);
    else delete (HTMLElement.prototype as unknown as { offsetWidth?: number }).offsetWidth;
  }
});

test("incomplete model pagination marks the known issue count as a lower bound", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 2, online: 2, offline: 0 } });
  api.modelsPage = async ({ page }: { page: number }) => ({
    items: page === 1 ? [{ id: 1, name: "failed model", presence: { failed: 1 } }] : [],
    total: 101,
  }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("link", { name: "Model file issues: at least 1" })));
  assert.ok(document.querySelector(".dashboard-overview-attention")?.textContent?.includes("Needs attention 1 category"));
  assert.ok(document.querySelector(".dashboard-overview-alert"));
  assert.ok(!document.querySelector(".dashboard-overview-attention")?.textContent?.includes("failed model"));
});

test("hundreds of issues across every category remain a seven-category strip with accurate node count", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 1000, online: 590, connecting: 10, offline: 400 } });
  api.nodesPage = async () => ({ items: Array.from({ length: 20 }, (_, index) => ({ name: `offline-${index}`, hostname: `offline-${index}` })), total: 400 }) as never;
  api.gpus = async () => ({ items: Array.from({ length: 80 }, (_, index) => ({ uuid: `gpu-${index}`, name: `faulty-${index}`, vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "unusable" })), overview: { total: 80, allocation: { unusable: 80 } } }) as never;
  api.monitorOverview = async () => ({ services: [
    ...Array.from({ length: 60 }, (_, index) => ({ deployment_id: index + 1, name: `service-${index}`, state: "running", health: { status: "failed" } })),
    { deployment_id: 61, name: "stopped", state: "stopped", health: { status: "failed" } },
  ] }) as never;
  api.modelsPage = async () => ({ items: Array.from({ length: 70 }, (_, index) => ({ id: index + 1, name: `model-${index}`, presence: { failed: 1 } })), total: 70 }) as never;
  api.storages = async () => ({ items: Array.from({ length: 40 }, (_, index) => ({ id: index + 1, name: `storage-${index}`, type: index % 2 ? "nfs" : "local", nodes: { problems: [{ state: "failed" }] } })), total: 40 }) as never;
  api.storage = async (id: number) => ({ id, name: `storage-${id - 1}`, type: "nfs", nodes: { all: [], problems: [{ state: "failed" }] } }) as never;
  api.providers = async () => ({ items: [
    ...Array.from({ length: 20 }, (_, index) => ({ id: index + 1, name: `public-${index}`, kind: "runpod", state: "invalid" })),
    ...Array.from({ length: 20 }, (_, index) => ({ id: index + 21, name: `private-${index}`, kind: "zstack", state: "invalid" })),
  ], total: 40 }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("button", { name: "5 more categories" })));
  const strip = document.querySelector(".dashboard-overview-attention")!;
  assert.ok(strip.textContent?.includes("Needs attention 7 categories"));
  assert.deepEqual([...strip.querySelectorAll("a")].map((link) => [link.textContent, link.getAttribute("href")]), [["Offline nodes 400", "/nodes"], ["Unavailable GPUs 80", "/gpus"]]);
  assert.ok(!strip.textContent?.includes("offline-0") && !strip.textContent?.includes("690"));
  fireEvent.click(screen.getByRole("button", { name: "5 more categories" }));
  await waitFor(() => assert.deepEqual(["/monitor", "/models", "/storage", "/providers", "/zstack"], [...document.querySelectorAll<HTMLAnchorElement>(".ant-dropdown-menu a")].map((link) => link.getAttribute("href"))));
  assert.deepEqual([...document.querySelectorAll<HTMLAnchorElement>(".ant-dropdown-menu a")].map((link) => link.textContent), ["Inference service issues 60", "Model file issues 70", "Storage issues 40", "Public cloud issues 20", "Private cloud issues 20"]);
  assert.equal(document.querySelectorAll(".ant-dropdown-menu a").length, 5);
});

test("default local storage stays in Storage and an overview with no shared storage has an honest empty state", async () => {
  stub();
  api.storages = async () => ({ items: [{ id: 1, name: "default", type: "local" }], total: 1 }) as never;
  let fetchedDetails = 0;
  api.storage = async () => { fetchedDetails++; throw new Error("local detail must stay in Storage"); };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("No shared storage")));
  assert.equal(fetchedDetails, 0);
  assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 0);
  assert.ok(screen.getByText("View local storage in Storage"));
  assert.ok(!document.querySelector(".dashboard-overview")?.textContent?.includes("Model cache"));
});

test("shared storage uses storage-level capacity once and leaves node cache details in Storage", async () => {
  stub();
  api.storages = async () => ({ items: [{ id: 2, name: "shared", type: "nfs_cache" }], total: 1 }) as never;
  api.storage = async () => ({ id: 2, name: "shared", type: "nfs_cache", total_bytes: 110 * 1024 ** 3, available_bytes: 40 * 1024 ** 3, nodes: { all: [
    { node_uuid: "a", node_name: "a", total_bytes: 100 * 1024 ** 3, available_bytes: 50 * 1024 ** 3, cache: { used_bytes: 1024 ** 3, reclaimable_bytes: 0 } },
    { node_uuid: "b", node_name: "b", total_bytes: 100 * 1024 ** 3, available_bytes: 50 * 1024 ** 3, cache: { used_bytes: 2 * 1024 ** 3, reclaimable_bytes: 0 } },
  ], problems: [{ node_uuid: "a", state: "offline" }] } }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Used 70.0 GiB / total 110.0 GiB")));
  assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 1);
  assert.ok(!document.querySelector(".dashboard-overview-storage-row")?.textContent?.includes("nodes with issues"));
  assert.ok(!document.querySelector(".dashboard-overview")?.textContent?.includes("Model cache"));
  assert.ok(!document.querySelector(".dashboard-overview")?.textContent?.includes("3.0 GiB"));
});

test("multiple shared storages stay in a compact two-row page with the full count visible", async () => {
  stub();
  api.storages = async () => ({ items: [1, 2, 3].map((id) => ({ id, name: `shared-${id}`, type: "nfs_cache" })), total: 3 }) as never;
  api.storage = async (id: number) => ({ id, name: `shared-${id}`, type: "nfs_cache", total_bytes: 100 * 1024 ** 3, available_bytes: id * 10 * 1024 ** 3, nodes: { all: [], problems: [] } }) as never;
  renderPage();
  const capacity = document.querySelector(".dashboard-overview-capacity")!;
  await waitFor(() => assert.equal(capacity.querySelectorAll(".dashboard-overview-storage-row").length, 2));
  assert.ok(capacity.textContent?.includes("3 shared storages"));
  assert.deepEqual([...capacity.querySelectorAll(".dashboard-overview-storage-row a")].map((link) => link.textContent), ["shared-1", "shared-2"]);
  assert.equal(capacity.querySelectorAll(".dashboard-overview-storage-progress").length, 2);
  assert.ok([...capacity.querySelectorAll(".dashboard-overview-storage-row")].every((row) => row.lastElementChild?.classList.contains("dashboard-overview-storage-progress")));
  fireEvent.click(capacity.querySelector(".ant-pagination-item-2")!);
  await waitFor(() => assert.deepEqual([...capacity.querySelectorAll(".dashboard-overview-storage-row a")].map((link) => link.textContent), ["shared-3"]));
  assert.ok(capacity.textContent?.includes("Used 70.0 GiB / total 100.0 GiB"));
});

test("one failed storage detail keeps the other storage's capacity visible and warns about incomplete data", async () => {
  stub();
  api.storages = async () => ({ items: [
    { id: 1, name: "broken", type: "nfs" },
    { id: 2, name: "healthy", type: "nfs_cache" },
  ], total: 2 }) as never;
  api.storage = async (id: number) => {
    if (id === 1) throw new Error("detail unavailable");
    return { id, name: "healthy", type: "nfs_cache", total_bytes: 100 * 1024 ** 3, available_bytes: 40 * 1024 ** 3,
      nodes: { all: [{ node_uuid: "a", node_name: "a", cache: { used_bytes: 2 * 1024 ** 3, reclaimable_bytes: 1024 ** 3 } }], problems: [] } } as never;
  };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Used 60.0 GiB / total 100.0 GiB")));
  assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 1);
  assert.ok(document.querySelector(".dashboard-overview-alert"));
});

test("unavailable shared storage details do not claim no shared storage is configured", async () => {
  stub();
  api.storages = async () => ({ items: [{ id: 1, name: "inaccessible", type: "nfs_cache" }], total: 1 }) as never;
  api.storage = async () => { throw new Error("detail unavailable"); };
  renderPage();
  await waitFor(() => assert.ok(document.querySelector(".dashboard-overview-alert")));
  const panel = document.querySelector(".dashboard-overview-capacity > .dashboard-overview-panel");
  assert.ok(panel?.textContent?.includes("Some overview data is temporarily unavailable"));
  assert.ok(!panel?.textContent?.includes("No shared storage"));
});

test("stopped services do not become attention items", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 2, online: 2, offline: 0 } });
  api.monitorOverview = async () => ({ services: [{ deployment_id: 2, name: "manually stopped", state: "stopped", health: { status: "failed" } }] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("H100")));
  assert.equal(document.querySelectorAll(".dashboard-overview-attention").length, 0);
});

test("GPU status counts remain independent of missing telemetry readings", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, current_metrics: { utilization_pct: null, temperature_c: null, power_watts: null, memory_used_bytes: null } });
  renderPage();
  await waitFor(() => assert.ok(document.querySelector(".dashboard-overview-resource")));
  const gpu = document.querySelectorAll(".dashboard-overview-resource")[1];
  await waitFor(() => assert.ok(gpu.textContent?.includes("Allocatable 1")));
  assert.ok(!gpu.textContent?.includes("Utilization"));
  assert.ok(!document.querySelector(".dashboard-overview-posture")?.textContent?.includes("Utilization"));
});


test("GPU resource counts use the shared fleet overview with partial devices counted only as allocatable", async () => {
  stub();
  const items = Array.from({ length: 71 }, (_, index) => ({
    uuid: `allocation-${index}`, name: "H100", vendor: "nvidia", memory_total_bytes: 80 * 1024 ** 3,
    allocation_state: index < 5 ? "allocated" : index < 7 ? "partial" : index < 30 ? "unusable" : "available",
  }));
  api.gpus = async () => ({ items, overview: { total: 71, allocation: { allocated: 5, allocatable: 43, unusable: 23, unknown: 0 } } }) as never;
  renderPage();
  await screen.findByText("Allocatable 43");
  const gpu = document.querySelectorAll(".dashboard-overview-resource")[1];
  assert.deepEqual([...gpu.querySelectorAll(".dashboard-overview-status")].map((status) => status.textContent), ["Allocatable 43", "Allocated 5", "Unavailable 23"]);
  assert.ok(gpu.textContent?.includes("Allocatable includes partially allocated GPUs."));
});
