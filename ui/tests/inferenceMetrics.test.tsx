import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { MetricSeries } from "../src/api";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// echarts needs a 2d context that JSDOM cannot provide; a catch-all proxy
// keeps chart mounting alive so panel behaviour stays testable.
const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 190, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")) } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InferenceMetricsPanel, percentScaled, ratioSeries } = await import("../src/components/InferenceMetricsPanel.tsx");
const originalMetricsQuery = api.metricsQuery;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
});

const LABELS = { instance: "inst-1", engine: "vllm", node: "node-1" };
function series(values: Array<[number, number]>): MetricSeries[] {
  return [{ labels: LABELS, points: values }];
}

function stubMetrics(byMetric: Record<string, MetricSeries[]>) {
  api.metricsQuery = (async (metric: string) => list(byMetric[metric] ?? [])) as typeof api.metricsQuery;
}

function summary() {
  return screen.getByRole("region", { name: "Instance Metrics" });
}

function card(title: string): HTMLElement {
  const heading = screen.getByText(title);
  return heading.closest('.deployment-trend-card') as HTMLElement;
}

test("ratioSeries divides counter windows and skips resets and unaligned samples", () => {
  const numerator = series([
    [1000, 10], [2000, 20], [3000, 35], [4000, 5], [5000, 6], [6000, 10],
  ]);
  // Both counters reset at 4000 (engine restart) and the denominator misses
  // the 5000 sample: the 3000→4000 transition window is dropped, the window
  // after the restart resumes, the unaligned sample is ignored.
  const denominator = series([
    [1000, 20], [2000, 40], [3000, 70], [4000, 1], [6000, 3],
  ]);
  const out = ratioSeries(numerator, denominator);
  assert.equal(out.length, 1);
  assert.deepEqual(out[0].points, [
    [2000, 0.5], [3000, 0.5], [6000, 2.5],
  ]);
});

test("percentScaled tolerates fraction and percentage gauges", () => {
  assert.equal(percentScaled(0.42), 42);
  assert.equal(percentScaled(1), 100);
  assert.equal(percentScaled(42), 42);
  assert.equal(percentScaled(100), 100);
});

test("vLLM counter data renders the KV card and leaves spec decode unavailable", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    // vLLM's cache gauge is a 0..1 fraction; the stat must render it as 55%.
    gpuworks_infer_gpu_cache_usage_percent: series([[1000, 0.55]]),
    gpuworks_infer_kv_hits_total: series([[1000, 100], [2000, 220]]),
    gpuworks_infer_kv_queries_total: series([[1000, 200], [2000, 400]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("KV Cache Hit Rate").querySelector("canvas")));
  assert.match(summary().textContent ?? "", /GPU Cache55%/, "cache gauge 0.55 must render as 55%");
  assert.ok(!card("KV Cache Hit Rate").textContent?.includes("Engine did not report this metric"));
  assert.ok(card("Speculative Accept Rate").textContent?.includes("Engine did not report this metric"));
  assert.ok(card("KV Cache Hit Rate").querySelector('[aria-label="Share of prompt tokens reused from the prefix cache"]'));
});

test("counters reported without traffic show the no-traffic state, not unreported", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    // vLLM emits both counter families from process start at zero; without
    // traffic no ratio window exists, which must read as "no traffic", not
    // as "engine did not report the metric".
    gpuworks_infer_kv_hits_total: series([[1000, 0], [2000, 0]]),
    gpuworks_infer_kv_queries_total: series([[1000, 0], [2000, 0]]),
    gpuworks_infer_spec_draft_tokens_total: series([[1000, 0], [2000, 0]]),
    gpuworks_infer_spec_accepted_tokens_total: series([[1000, 0], [2000, 0]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("KV Cache Hit Rate").textContent?.includes("no qualifying traffic yet")));
  assert.ok(card("Speculative Accept Rate").textContent?.includes("no qualifying traffic yet"));
  assert.ok(!card("KV Cache Hit Rate").textContent?.includes("Engine did not report this metric"));
});

test("SGLang instant gauges fall back when counter pairs are absent", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    gpuworks_infer_kv_hit_rate_instant: series([[1000, 0.42]]),
    gpuworks_infer_spec_accept_rate: series([[1000, 0.81]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("KV Cache Hit Rate").querySelector("canvas")));
  assert.ok(card("Speculative Accept Rate").querySelector("canvas"));
  assert.ok(!card("KV Cache Hit Rate").textContent?.includes("Engine did not report this metric"));
});

test("a panel without any engine metrics keeps showing the waiting alert", async () => {
  stubMetrics({});
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);

  await waitFor(() => assert.ok(screen.getByText("Waiting for Inference Metrics")));
  assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-value-row']")].map(el => el.textContent), Array(5).fill("—"));
  assert.ok(screen.getByText("Stage Duration p95"));
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  assert.ok(await screen.findByText("KV Cache Hit Rate"));
});

test("missing numeric metrics stay unknown while an observed zero remains zero", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 2]]),
    gpuworks_infer_requests_waiting: series([[1000, 0]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-value-row']")].map(el => el.textContent), ["2", "0", "—", "—", "—"]));
  assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-label']")].map(el => el.textContent), [
    "Running Requests", "Waiting Requests", "GPU Cache", "Prompt Throughput", "Generation Throughput",
  ]);
});

test("failed metric reads remain explicit and do not suppress the other diagnostics", async () => {
  api.metricsQuery = async (metric) => {
    if (metric.endsWith("requests_waiting")) throw new Error("read unavailable");
    return list([]);
  };
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  assert.ok(await screen.findByText("read unavailable"));
  assert.equal(screen.queryByText("Waiting for Inference Metrics"), null);
  assert.ok(screen.getByText("Stage Duration p95"));
});

// The gateway counts what a deployment served, not what each instance did, so
// the instance page never borrows a deployment-wide figure for an instance:
// where its engine reports nothing it says so and points to the deployment's
// panel, which does show the gateway's numbers.
test("an engine without token counters says the gateway cannot split them by instance", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    gpuworks_infer_gpu_cache_usage_percent: series([[1000, 0.5]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.match(card("Token Throughput").textContent ?? "", /Engine did not report token counters\. The gateway counts per deployment, not per instance/));
  assert.equal(card("Token Throughput").querySelector("canvas"), null);
  assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-value-row']")].slice(3).map(el => el.textContent), ["—", "—"], "no deployment-wide number is shown as this instance's throughput");
});

test("an engine that reports token counters draws them, with no unreported message", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    gpuworks_infer_prompt_tokens_total: series([[1000, 0], [11000, 500]]),
    gpuworks_infer_generation_tokens_total: series([[1000, 0], [11000, 2000]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.ok(card("Token Throughput").querySelector("canvas")));
  assert.ok(!card("Token Throughput").textContent?.includes("did not report token counters"));
  await waitFor(() => assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-value-row']")].slice(3).map(el => el.textContent), ["50.0tok/s", "200.0tok/s"]));
});

test("an unreported prefix-cache hit rate and stage times point to the deployment, speculative decoding does not", async () => {
  stubMetrics({ gpuworks_infer_requests_running: series([[1000, 1]]) });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.match(card("Stage Duration p95").textContent ?? "", /gateway summarizes stage times per deployment/));
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  assert.match(card("KV Cache Hit Rate").textContent ?? "", /deployment-wide figure is in the deployment metrics/);
  assert.ok(!card("Speculative Accept Rate").textContent?.includes("deployment"), "the gateway has no speculative-decoding figure");
});

// What the chart actually holds: ECharts' resolved option for the chart in a
// card, read back from the live instance behind it.
async function chartOption(title: string) {
  const echarts = await import("echarts/core");
  const host = card(title).querySelector<HTMLElement>('[role="img"]');
  assert.ok(host, `${title} has a chart`);
  const instance = echarts.getInstanceByDom(host);
  assert.ok(instance, `${title} chart is mounted`);
  return instance.getOption() as {
    xAxis: Array<{ min?: number; max?: number }>;
    yAxis: Array<{ min?: number; max?: number; interval?: number }>;
    series: Array<{ showSymbol?: boolean; data: Array<[number, number]> }>;
  };
}

// recordWindows answers like stubMetrics and remembers each query's bounds.
function recordWindows(byMetric: Record<string, MetricSeries[]>) {
  const asked = new Map<string, { start: number; end: number }>();
  api.metricsQuery = (async (metric: string, params: { start?: number; end?: number } = {}) => {
    asked.set(metric, { start: params.start!, end: params.end! });
    return list(byMetric[metric] ?? []);
  }) as typeof api.metricsQuery;
  return asked;
}

test("a single speculative-accept ratio point draws a visible point on the window the query asked for", async () => {
  const at = Date.now() - 5_000;
  // Both counters moved within one scrape interval: one ratio window, 0.753.
  const asked = recordWindows({
    gpuworks_infer_requests_running: series([[at, 1]]),
    gpuworks_infer_spec_accepted_tokens_total: series([[at - 15_000, 0], [at, 753]]),
    gpuworks_infer_spec_draft_tokens_total: series([[at - 15_000, 0], [at, 1000]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("Speculative Accept Rate").querySelector("canvas")));
  assert.ok(!card("Speculative Accept Rate").textContent?.includes("no qualifying traffic yet"), "one ratio point is data, not the empty state");
  const option = await chartOption("Speculative Accept Rate");
  const window = asked.get("gpuworks_infer_spec_accepted_tokens_total");
  assert.ok(window);
  assert.equal(window.end - window.start, 15 * 60_000);
  assert.deepEqual([option.xAxis[0].min, option.xAxis[0].max], [window.start, window.end], "x axis is the 15 minute window, not a day either side of the point");
  assert.deepEqual([option.yAxis[0].min, option.yAxis[0].max], [0, 100], "a ratio is read against 0..100 %");
  assert.equal(option.series[0].showSymbol, true, "a lone point has no segment, so it shows its symbol");
  assert.equal(option.series[0].data.length, 1);
  assert.equal(option.series[0].data[0][0], at);
  assert.ok(Math.abs(option.series[0].data[0][1] - 75.3) < 1e-9, "0.753 is drawn as 75.3 on the 0..100 axis");
});

test("the cache charts hold a 0..100 axis over the query window, whether the engine reports fractions or percentages", async () => {
  const at = Date.now() - 5_000;
  const asked = recordWindows({
    gpuworks_infer_requests_running: series([[at, 1]]),
    // vLLM: 0..1 fractions. Both are one reading: a fitted axis would pad around it.
    gpuworks_infer_gpu_cache_usage_percent: series([[at, 0.55]]),
    gpuworks_infer_kv_hits_total: series([[at - 15_000, 100], [at, 120]]),
    gpuworks_infer_kv_queries_total: series([[at - 15_000, 200], [at, 240]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  await waitFor(() => assert.ok(card("KV Cache Hit Rate").querySelector("canvas")));
  for (const [title, metric, value] of [["GPU Cache Usage", "gpuworks_infer_gpu_cache_usage_percent", 55], ["KV Cache Hit Rate", "gpuworks_infer_kv_hits_total", 50]] as const) {
    const option = await chartOption(title);
    const window = asked.get(metric)!;
    assert.deepEqual([option.xAxis[0].min, option.xAxis[0].max], [window.start, window.end], title);
    assert.deepEqual([option.yAxis[0].min, option.yAxis[0].max, option.yAxis[0].interval], [0, 100, 20], title);
    assert.ok(Math.abs(option.series[0].data[0][1] - value) < 1e-9, `${title} ${option.series[0].data[0][1]}`);
    assert.equal(option.series[0].showSymbol, true, `${title} has a single point`);
  }
});

test("a multi-point instance chart keeps its line without symbols and takes the query window", async () => {
  const at = Date.now() - 5_000;
  const asked = recordWindows({
    gpuworks_infer_requests_running: series([[at - 20_000, 1], [at - 10_000, 2], [at, 3]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.ok(card("Instance Request Concurrency").querySelector("canvas")));
  const option = await chartOption("Instance Request Concurrency");
  const window = asked.get("gpuworks_infer_requests_running")!;
  assert.deepEqual([option.xAxis[0].min, option.xAxis[0].max], [window.start, window.end]);
  assert.equal(option.series[0].showSymbol, false);
  assert.equal(option.series[0].data.length, 3);
});

test("stage-duration and preemption charts take the query window too", async () => {
  const at = Date.now() - 5_000;
  const asked = recordWindows({
    gpuworks_infer_requests_running: series([[at, 1]]),
    gpuworks_infer_queue_time_seconds: [{ labels: { ...LABELS, quantile: "0.95" }, points: [[at, 0.2]] }],
    gpuworks_infer_preemptions_total: series([[at - 10_000, 0], [at, 3]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.ok(card("Stage Duration p95").querySelector("canvas")));
  await waitFor(() => assert.ok(card("Preemptions /s").querySelector("canvas")));
  const stage = await chartOption("Stage Duration p95");
  const queue = asked.get("gpuworks_infer_queue_time_seconds")!;
  assert.deepEqual([stage.xAxis[0].min, stage.xAxis[0].max], [queue.start, queue.end]);
  assert.equal(stage.series[0].showSymbol, true);
  const preempt = await chartOption("Preemptions /s");
  const preemptWindow = asked.get("gpuworks_infer_preemptions_total")!;
  assert.deepEqual([preempt.xAxis[0].min, preempt.xAxis[0].max], [preemptWindow.start, preemptWindow.end]);
});
