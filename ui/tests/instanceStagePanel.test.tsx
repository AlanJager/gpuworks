import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { MonitorDeployment, MonitorSeries } from "../src/api";

// The server's deployment snapshot, generated from the real handler.
const monitorFixture = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-deployment.json", import.meta.url), "utf8")) as MonitorDeployment;

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// The "data exists" branch mounts ECharts (via TimeSeriesChart); jsdom has no
// 2d context, so stub one the same way inferenceMetrics.test.tsx does.
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
const { default: InstanceStagePanel, stageSeriesName } = await import("../src/components/InstanceStagePanel.tsx");
const originalMonitorDeployment = api.monitorDeployment;
const t = i18n.getFixedT("en", "deployments");

afterEach(() => {
  cleanup();
  api.monitorDeployment = originalMonitorDeployment;
});

function emptyCharts(): MonitorDeployment["charts"] {
  return { latency: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] };
}

function snapshot(charts: Partial<MonitorDeployment["charts"]>): MonitorDeployment {
  return {
    as_of: 0,
    stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null,
    ttft_breakdown: null,
    charts: { ...emptyCharts(), ...charts },
    replicas: [],
  };
}

function stubMonitorDeployment(charts: Partial<MonitorDeployment["charts"]>) {
  api.monitorDeployment = (async () => snapshot(charts)) as typeof api.monitorDeployment;
}

function series(labels: Record<string, string>, values: Array<[number, number]>): MonitorSeries {
  return { labels, points: values };
}

test("neither stage p95 nor preemption rate are collected today, so both cards say so", async () => {
  stubMonitorDeployment({});
  render(<I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid="inst-1" deploymentId={1} /></I18nextProvider>);
  await waitFor(() => assert.equal(screen.getAllByText(/Not collected yet/).length, 2));
  assert.ok(screen.getByText("Stage Duration p95"));
  assert.ok(screen.getByText("Preemptions /s"));
});

test("a stage_time series labelled for this instance renders a chart, not the not-collected card", async () => {
  stubMonitorDeployment({
    stage_time: [series({ instance: "inst-1", metric: "queue" }, [[1000, 12], [2000, 18]])],
  });
  render(<I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid="inst-1" deploymentId={1} /></I18nextProvider>);
  await waitFor(() => assert.equal(screen.queryAllByText(/Not collected yet/).length, 1));
  assert.ok(screen.getByText("Preemptions /s"), "the preemption card still has no data of its own");
});

test("a chart labelled for a different instance is filtered out, leaving the not-collected state", async () => {
  stubMonitorDeployment({
    stage_time: [series({ instance: "inst-other", metric: "queue" }, [[1000, 12], [2000, 18]])],
  });
  render(<I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid="inst-1" deploymentId={1} /></I18nextProvider>);
  await waitFor(() => assert.equal(screen.getAllByText(/Not collected yet/).length, 2));
});

test("a chart with no instance label at all is shown unfiltered rather than hidden", async () => {
  stubMonitorDeployment({
    preemptions: [series({ deployment: "1" }, [[1000, 10], [2000, 20], [3000, 25]])],
  });
  render(<I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid="inst-1" deploymentId={1} /></I18nextProvider>);
  await waitFor(() => assert.equal(screen.queryAllByText(/Not collected yet/).length, 1));
  assert.ok(screen.getByText("Stage Duration p95"), "the stage card still has no data of its own");
});

test("the panel polls the deployment snapshot by deploymentId, not by instance", async () => {
  const requested: number[] = [];
  api.monitorDeployment = (async (id: number) => { requested.push(id); return snapshot({}); }) as typeof api.monitorDeployment;
  render(<I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid="inst-1" deploymentId={42} /></I18nextProvider>);
  await waitFor(() => assert.deepEqual(requested, [42]));
});

// The stage_time chart carries three lines distinguished only by
// labels.metric, named as the server's deployment snapshot names them (the
// generated fixture, internal/server/monitorapi). Matching names the wire
// never carried left the legend reading "metric queue_ms".
test("stageSeriesName names the wire's stage lines through the translated stage vocabulary", () => {
  const wire = (monitorFixture.charts.stage_time as Array<{ labels: Record<string, string> }>).map((series) => series.labels);
  assert.deepEqual(wire.map((labels, index) => stageSeriesName(labels, index, t)), ["Queue", "Prefill", "Decode"]);
});

test("stageSeriesName falls back to defaultSeriesName for an unrecognized or absent metric label", () => {
  assert.equal(stageSeriesName({ instance: "inst-1" }, 0, t), "inst-1");
  assert.equal(stageSeriesName({ metric: "other" }, 3, t), "other");
});
