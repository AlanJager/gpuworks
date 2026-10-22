import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";

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

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;

const { api } = await import("../src/api.ts");
const { default: DeploymentMetricsPanel } = await import("../src/components/DeploymentMetricsPanel.tsx");
const originalMetricsQuery = api.metricsQuery;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
});

const runningInstance = { uuid: "inst-range-1", state: "running", node_uuid: "node-1" } as never;

function renderPanel() {
  return render(
    <I18nextProvider i18n={i18n}><DeploymentMetricsPanel instances={[runningInstance]} /></I18nextProvider>,
  );
}

// The metrics stub returns no points, so the panel shows its waiting state
// with the range control; the queries themselves are what this file asserts.
function recordMetricsQuery() {
  const calls: Array<Record<string, string>> = [];
  api.metricsQuery = (async (metric: string, params: Record<string, string>) => {
    calls.push({ metric, ...params });
    return list([]);
  }) as typeof api.metricsQuery;
  return calls;
}

async function chooseRange(label: string) {
  const user = userEvent.setup({ document: dom.window.document });
  const combo = screen.getByRole("combobox", { name: "Time range" });
  fireEvent.mouseDown(combo);
  const option = await screen.findByText(label, { selector: ".ant-select-item-option-content" });
  await user.click(option);
}

test("the default 15-minute window queries a 10s step for every metric", async () => {
  const calls = recordMetricsQuery();
  renderPanel();

  await waitFor(() => assert.ok(calls.length >= 5, `expected the metric fan-out, got ${calls.length}`));
  for (const call of calls) {
    assert.equal(call.step, "10s", `unexpected step for ${call.metric}`);
    const windowMS = Number(call.end) - Number(call.start);
    assert.ok(Math.abs(windowMS - 15 * 60 * 1000) < 5_000, `window ${windowMS}ms is not ~15m`);
    assert.equal(call.instance, "inst-range-1");
  }
  // No points came back, so the panel explains it is waiting rather than
  // rendering empty charts.
  assert.ok(screen.getByText("Waiting for Deployment Metrics"));
});

test("picking a quick range widens the window and coarsens the step", async () => {
  const calls = recordMetricsQuery();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 5));
  calls.length = 0;

  await chooseRange("1 hour");

  await waitFor(() => assert.ok(calls.length >= 5, `re-query did not happen: ${calls.length}`));
  for (const call of calls) {
    assert.equal(call.step, "30s", `unexpected step for ${call.metric}`);
    const windowMS = Number(call.end) - Number(call.start);
    assert.ok(Math.abs(windowMS - 60 * 60 * 1000) < 5_000, `window ${windowMS}ms is not ~1h`);
  }
});

test("a range beyond retention keeps querying but shows the warning", async () => {
  const calls = recordMetricsQuery();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 5));
  calls.length = 0;

  await chooseRange("1 month");

  assert.ok(await screen.findByText(/exceeds the 7-day retention/), "retention warning missing");
  await waitFor(() => assert.ok(calls.length >= 5, "1 month must still query the retained window"));
});
