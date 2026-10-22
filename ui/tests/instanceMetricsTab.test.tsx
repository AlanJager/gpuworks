import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { Instance, MetricSeries, MonitorDeployment } from "../src/api";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

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

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstanceMetricsTab } = await import("../src/pages/InstanceMetricsTab.tsx");
const originalMetricsQuery = api.metricsQuery;
const originalMonitorDeployment = api.monitorDeployment;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
  api.monitorDeployment = originalMonitorDeployment;
});

function stubMetrics(byMetric: Record<string, MetricSeries[]>) {
  api.metricsQuery = (async (metric: string) => list(byMetric[metric] ?? [])) as typeof api.metricsQuery;
}

function stubMonitorDeployment() {
  api.monitorDeployment = (async () => ({
    as_of: 0,
    stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null,
    ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  } satisfies MonitorDeployment)) as typeof api.monitorDeployment;
}

function baseInstance(overrides: Partial<Instance> = {}): Instance {
  return {
    uuid: "inst-1", deployment_id: 1, node_uuid: "node-1", state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "", container_id: "container-1", sleep_level: 0,
    ...overrides,
  } as Instance;
}

test("a stopped instance shows the no-metrics placeholder instead of stale panels", async () => {
  stubMetrics({});
  stubMonitorDeployment();
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={baseInstance({ state: "stopped" })} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("The instance is not running; no inference metrics are available")));
});

test("a running instance's metrics tab includes the KV tier panel and the GPU strip", async () => {
  stubMetrics({});
  stubMonitorDeployment();
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={baseInstance()} params={{ kv_offloading_size: 8 }} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("KV Tiers")));
  assert.ok(screen.getByText("8 GiB"), "KVTierPanel reads the deployment params passed through");
  assert.ok(screen.getByText("Per-GPU Telemetry"));
  assert.ok(screen.getByText("Stage Duration p95"));
  assert.ok(screen.getByText("Preemptions /s"));
});
