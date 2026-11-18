import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const load = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { networks: load("networks"), common: load("common") } },
  defaultNS: "common",
  ns: ["networks", "common"],
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const NetworkTestResult = (await import("../src/pages/networks/NetworkTestResult.tsx")).default;
const CardTestResult = (await import("../src/pages/networks/CardTestResult.tsx")).default;
const detailFixture = JSON.parse(fs.readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));

after(() => { closeChannels(); dom.window.close(); });
afterEach(() => cleanup());

type Scope = { scope: "all" | "nodes" | "label"; label_key?: string; label_value?: string };

// A finished run as the server returns it: label_key and label_value only on a label run.
function finishedRun(kind: string, pattern: string, scope: Scope) {
  return {
    id: 7, status: "done", kind, pattern, ...scope, bandwidth: false, node_count: 2, pair_count: 0, passed_count: 0, partial_count: 0,
    rail_count: 0, rail_passed_count: 0, node_uuids: ["a", "b"], pairs: [], started_at: "2026-09-30T04:00:00Z", finished_at: "2026-09-30T04:00:10Z",
  } as never;
}

const renderIn = (element: React.ReactElement) => render(<I18nextProvider i18n={i18n}>{element}</I18nextProvider>);

/** The card title that carries the run's summary, found by the scope it names. */
function summaryTitle(scopeText: RegExp) {
  return screen.getByText(scopeText).textContent ?? "";
}

const scopes: { scope: Scope; shown: RegExp }[] = [
  { scope: { scope: "label", label_key: "rack", label_value: "r2" }, shown: /· label rack=r2 ·/ },
  { scope: { scope: "nodes" }, shown: /· selected nodes ·/ },
  { scope: { scope: "all" }, shown: /· all nodes ·/ },
];

test("a network test result names the label it ran on, and nothing else for other scopes", () => {
  for (const { scope, shown } of scopes) {
    const view = renderIn(<NetworkTestResult network={detailFixture} run={finishedRun("tcp", "mesh", scope)} onChanged={() => {}} />);
    const title = summaryTitle(shown);
    assert.match(title, /^TCP/);
    assert.doesNotMatch(title, /undefined/);
    if (scope.scope !== "label") assert.doesNotMatch(title, /=/);
    view.unmount();
  }
});

test("a card test result names the label it ran on, and nothing else for other scopes", () => {
  for (const { scope, shown } of scopes) {
    const view = renderIn(<CardTestResult run={finishedRun("card_ping", "ring", scope)} />);
    const title = summaryTitle(shown);
    assert.doesNotMatch(title, /undefined/);
    if (scope.scope !== "label") assert.doesNotMatch(title, /=/);
    view.unmount();
  }
});

test("mesh filters combine the two node names with any selected test statuses, and issues open their pairs", async () => {
  const { fireEvent, within } = await import("@testing-library/react");
  const testPair = (source: string, target: string, status: string) => ({
    source_uuid: source, source_name: source, target_uuid: target, target_name: target,
    status, reachable: status !== "failed", passed: status === "passed",
    gbps: status === "failed" ? 0 : 180, avg_rail_gbps: status === "failed" ? undefined : 90, rtt_ms_p50: 0, rtt_ms_p99: 0,
    ...(status === "failed" ? { code: "offline", side: "target" } : {}),
    rails: status === "partial" ? [{ rail: "rail-a", gbps: 90 }, { rail: "rail-b", gbps: 0, code: "probe_failed" }] : [],
  });
  const pairs = [testPair("gpu-a", "gpu-b", "passed"), testPair("gpu-a", "gpu-c", "partial"), testPair("gpu-b", "gpu-c", "failed")];
  const run = { ...detailFixture.latest_test, id: 10, kind: "rdma_tool", pattern: "mesh", bandwidth: true, pairs,
    pair_count: 3, passed_count: 1, partial_count: 1, node_count: 3 };
  renderIn(<NetworkTestResult network={detailFixture} run={run} onChanged={() => {}} />);
  const matrix = () => screen.getByRole("table", { name: "Global matrix" });
  const cells = () => within(matrix()).getAllByRole("button");
  assert.equal(cells().length, 3);
  fireEvent.click(screen.getByRole("checkbox", { name: "Failed", exact: true }));
  assert.equal(cells().length, 2);
  fireEvent.click(screen.getByRole("checkbox", { name: "All passed" }));
  assert.equal(cells().length, 1);
  assert.equal(within(matrix()).getAllByRole("rowheader").length, 1);
  assert.equal(within(matrix()).getAllByRole("columnheader").length, 2);
  const target = screen.getByRole("searchbox", { name: "Filter target nodes" });
  fireEvent.change(target, { target: { value: "GPU-C" } });
  assert.match(cells()[0].getAttribute("aria-label") ?? "", /gpu-a → gpu-c: 90.0 Gb\/s · 1\/2 rails passed/);
  fireEvent.change(screen.getByRole("searchbox", { name: "Filter source nodes" }), { target: { value: "gpu-b" } });
  assert.ok(screen.getByText("No matching pairs. Adjust the node names or status filters."));
  fireEvent.click(screen.getByRole("tab", { name: /Issue summary.*2/ }));
  const expand = await screen.findAllByRole("button", { name: /expand row/i });
  fireEvent.click(expand[0]);
  const failingPair = await screen.findByRole("button", { name: /gpu-a → gpu-c/ });
  fireEvent.click(failingPair);
  const details = await screen.findByRole("dialog");
  assert.ok(within(details).getByText("rail-b"));
});
