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
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { models: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/models.json", import.meta.url), "utf8")) } },
  defaultNS: "models",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { ModelPresenceSummary } = await import("../src/pages/models/ModelPresenceSummary.tsx");
const userEvent = (await import("@testing-library/user-event")).default;
const { modelPresenceBytes } = await import("../src/pages/models/modelPresenceDisplay.ts");

afterEach(() => cleanup());

const counts = (over: Partial<Record<string, number>> = {}) => ({ ready_nodes: 0, cached_nodes: 0, ready_storages: 0, in_flight: 0, in_flight_percent: 0, failed: 0, ...over });

test("a hundred nodes read as one summary line that opens the per-node files", async () => {
  let opened = 0;
  render(<I18nextProvider i18n={i18n}><ModelPresenceSummary presence={counts({ ready_nodes: 96, ready_storages: 1, cached_nodes: 1, in_flight: 2, in_flight_percent: 50, failed: 1 })} onOpen={() => { opened += 1; }} /></I18nextProvider>);
  const open = screen.getByRole("button", { name: "Ready on 96 nodes · 1 shared storages · 1 cached" });
  assert.ok(screen.getByText("2 transferring 50%"));
  assert.ok(screen.getByText("1 failed"));
  await userEvent.setup().click(open);
  assert.equal(opened, 1);
});

test("a model held only by a shared storage names the storage, not zero nodes", () => {
  render(<I18nextProvider i18n={i18n}><ModelPresenceSummary presence={counts({ ready_storages: 1 })} onOpen={() => {}} /></I18nextProvider>);
  assert.ok(screen.getByRole("button", { name: "1 shared storages" }));
});

test("a model only failing somewhere still links to its files", () => {
  render(<I18nextProvider i18n={i18n}><ModelPresenceSummary presence={counts({ failed: 2 })} onOpen={() => {}} /></I18nextProvider>);
  assert.ok(screen.getByRole("button", { name: "Ready on 0 nodes" }));
  assert.ok(screen.getByText("2 failed"));
});

test("a model in no location says it is not downloaded", () => {
  render(<I18nextProvider i18n={i18n}><ModelPresenceSummary presence={counts()} onOpen={() => {}} /></I18nextProvider>);
  assert.ok(screen.getByText("Not downloaded"));
});

test("model presence byte label handles unknown totals", () => {
  assert.equal(modelPresenceBytes({ downloaded: 1024, size_bytes: 0 }), "1.0 KiB");
  assert.equal(modelPresenceBytes({ downloaded: 0, size_bytes: 0 }), "—");
});
