import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/monitor/d/7" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
// The result filter's tag overflow schedules through MessageChannel.
const closeChannels = trackMessageChannels();
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

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

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: LatencyBreakdown } = await import("../src/pages/monitor/LatencyBreakdown.tsx");
import type { MonitorLatency } from "../src/monitorRequestsApi";

const original = api.monitorLatency;
afterEach(() => {
  cleanup();
  api.monitorLatency = original;
});
after(() => {
  closeChannels();
  dom.window.close();
});

function breakdown(over: Partial<MonitorLatency> = {}): MonitorLatency {
  return {
    from_ms: 0, to_ms: 1, requests: 40,
    end_to_end: { mean_ms: 4000, p50_ms: 3500, p95_ms: 9000 },
    segments: [
      { kind: "gateway", mean_ms: 200, requests: 40, p50_ms: 150, p95_ms: 400 },
      { kind: "queue", mean_ms: 600, requests: 30, p50_ms: 500, p95_ms: 2500 },
      { kind: "prefill", mean_ms: 400, requests: 40, p50_ms: 350, p95_ms: 900 },
      { kind: "thinking", mean_ms: 1000, requests: 20, p50_ms: 1800, p95_ms: 4000 },
      { kind: "answering", mean_ms: 1800, requests: 40, p50_ms: 1700, p95_ms: 3000 },
    ],
    speeds: { prefill_tok_s: 5200, thinking_tok_s: 61.4, answering_tok_s: null, decode_tok_s: 58 },
    ...over,
  };
}

function renderBreakdown(pd = false) {
  return render(
    <I18nextProvider i18n={i18n}>
      <LatencyBreakdown deploymentId={7} range="6h" pd={pd} />
    </I18nextProvider>,
  );
}

test("the bar splits the end-to-end mean by phase, in proportion, with the mean and percentiles on top", async () => {
  const calls: Array<[number, string]> = [];
  api.monitorLatency = async (id, range) => {
    calls.push([id, range]);
    return breakdown();
  };
  renderBreakdown();
  const bar = await screen.findByRole("list", { name: "Mean time per phase" });
  assert.deepEqual(calls[0], [7, "6h"]);
  assert.ok(screen.getByText("4 s"));
  assert.ok(screen.getByText("end-to-end mean · p50 3.5 s · p95 9 s · 40 requests"));
  const segments = within(bar).getAllByRole("listitem");
  assert.deepEqual(segments.map((s) => s.getAttribute("data-kind")), ["gateway", "queue", "prefill", "thinking", "answering"]);
  // Lengths follow the means, which add up to the end-to-end mean.
  assert.deepEqual(segments.map((s) => (s as HTMLElement).style.flexGrow), ["200", "600", "400", "1000", "1800"]);
});

test("focusing a phase shows its share, p50 / p95 and mean speed", async () => {
  api.monitorLatency = async () => breakdown();
  renderBreakdown();
  const bar = await screen.findByRole("list", { name: "Mean time per phase" });
  within(bar).getByRole("listitem", { name: "Prefill 400 ms" }).focus();
  await waitFor(() => assert.ok(screen.getByText("mean 400 ms, 10% of the total")));
  assert.ok(screen.getByText("p50 350 ms · p95 900 ms (40 requests had this phase)"));
  assert.ok(screen.getByText("mean speed 5200 tokens/s"));
  // Without an answer-only speed, answering quotes the whole decode.
  await userEvent.setup().hover(within(bar).getByRole("listitem", { name: /^Answer / }));
  await waitFor(() => assert.ok(screen.getByText("mean speed 58 tokens/s")));
});

test("empty range and a failed load with retry", async () => {
  let fail = true;
  api.monitorLatency = async () => {
    if (fail) throw new Error("ux.db locked");
    return breakdown({ requests: 0, end_to_end: null, segments: [] });
  };
  renderBreakdown();
  await screen.findByText("ux.db locked");
  fail = false;
  await userEvent.setup().click(screen.getByRole("button", { name: /Retry/ }));
  await screen.findByText("No whole reply in this range yet");
  assert.equal(screen.queryByRole("list", { name: "Mean time per phase" }), null);
});

// A PD deployment's breakdown has the prefill stage the gateway waited for
// after the gateway, and names the decode instance's queue and prefill as PD
// has them.
test("a PD deployment shows the prefill stage and the PD names of queue and prefill", async () => {
  const base = breakdown();
  api.monitorLatency = async () =>
    breakdown({ segments: [base.segments[0], { kind: "pd_prefill", mean_ms: 700, requests: 40, p50_ms: 650, p95_ms: 1200 }, ...base.segments.slice(1)] });
  renderBreakdown(true);
  const bar = await screen.findByRole("list", { name: "Mean time per phase" });
  assert.deepEqual(within(bar).getAllByRole("listitem").map((s) => s.getAttribute("data-kind")), ["gateway", "pd_prefill", "queue", "prefill", "thinking", "answering"]);
  // The breakdown calls the gateway-measured stage plain Prefill.
  assert.ok(within(bar).getByRole("listitem", { name: "Prefill 700 ms" }));
  assert.equal(within(bar).queryByRole("listitem", { name: /PD prefill/ }), null);
  assert.ok(within(bar).getByRole("listitem", { name: "KV transfer & queue 600 ms" }));
  assert.ok(within(bar).getByRole("listitem", { name: "First-token recompute 400 ms" }));
  // The legend uses the same names.
  assert.ok(screen.getByText("First-token recompute"));
  assert.equal(screen.queryByText("PD prefill"), null);
  assert.equal(screen.getAllByText("Prefill").length, 1);
  // The hover detail too.
  within(bar).getByRole("listitem", { name: "Prefill 700 ms" }).focus();
  await waitFor(() => assert.ok(screen.getByText("mean 700 ms, 18% of the total")));
  // The prompt is computed in the stage, so the prefill speed is quoted
  // there and not on the decode instance's recompute.
  await waitFor(() => assert.ok(screen.queryByText("mean speed 5200 tokens/s") != null));
  assert.ok((screen.getByText("mean 700 ms, 18% of the total").parentElement?.textContent ?? "").includes("mean speed 5200 tokens/s"));
  within(bar).getByRole("listitem", { name: "First-token recompute 400 ms" }).focus();
  const recompute = await screen.findByText("mean 400 ms, 10% of the total");
  assert.ok(!(recompute.parentElement?.textContent ?? "").includes("mean speed"), "no speed on the recompute");
});

// pdWithRecompute is a PD breakdown whose decode-side recompute has the given
// mean; the end-to-end mean stays 4000 ms.
function pdWithRecompute(recomputeMS: number): MonitorLatency {
  const base = breakdown();
  const segments = base.segments.map((s) => (s.kind === "prefill" ? { ...s, mean_ms: recomputeMS } : s));
  return breakdown({ segments: [segments[0], { kind: "pd_prefill", mean_ms: 700, requests: 40, p50_ms: 650, p95_ms: 1200 }, ...segments.slice(1)] });
}

async function renderedKinds(data: MonitorLatency, pd: boolean) {
  api.monitorLatency = async () => data;
  renderBreakdown(pd);
  const bar = await screen.findByRole("list", { name: "Mean time per phase" });
  return within(bar).getAllByRole("listitem").map((s) => s.getAttribute("data-kind"));
}

// The decode instance only recomputes the last prompt token, so a recompute
// of at most 1% of the end-to-end mean is left out of the bar and the legend.
test("a PD recompute under 1% of the end-to-end mean is not shown", async () => {
  assert.deepEqual(await renderedKinds(pdWithRecompute(39), true), ["gateway", "pd_prefill", "queue", "thinking", "answering"]);
  assert.equal(screen.queryByText("First-token recompute"), null);
  assert.equal(screen.queryByRole("listitem", { name: /First-token recompute/ }), null);
});

test("a PD recompute at exactly 1% of the end-to-end mean is not shown", async () => {
  assert.deepEqual(await renderedKinds(pdWithRecompute(40), true), ["gateway", "pd_prefill", "queue", "thinking", "answering"]);
  assert.equal(screen.queryByText("First-token recompute"), null);
});

test("a PD recompute just above 1% of the end-to-end mean is shown", async () => {
  assert.deepEqual(await renderedKinds(pdWithRecompute(41), true), ["gateway", "pd_prefill", "queue", "prefill", "thinking", "answering"]);
  assert.ok(screen.getByRole("listitem", { name: "First-token recompute 41 ms" }));
  assert.ok(screen.getByText("First-token recompute"));
});

test("a non-PD deployment always shows its prefill, however small", async () => {
  const base = breakdown();
  const data = breakdown({ segments: base.segments.map((s) => (s.kind === "prefill" ? { ...s, mean_ms: 4 } : s)) });
  assert.deepEqual(await renderedKinds(data, false), ["gateway", "queue", "prefill", "thinking", "answering"]);
  assert.ok(screen.getByRole("listitem", { name: "Prefill 4 ms" }));
  assert.ok(screen.getByText("Prefill"));
});
