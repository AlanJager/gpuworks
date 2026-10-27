import assert from "node:assert/strict";
import { listPage } from "./helpers/listPage.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";


const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});


const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");

// Faithful to the server's SSE frames: a named event whose data is {key, reason}.
class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
  static emit(type: string, reason: string) {
    const event = new dom.window.MessageEvent(type, { data: JSON.stringify({ key: "k", reason }) });
    for (const listener of Events.listeners.get(type) ?? []) listener(event);
  }
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { default: DeploymentsPage } = await import("../src/pages/DeploymentsPage.tsx");
const originalDeployments = api.deploymentsPage;
afterEach(() => { cleanup(); Events.listeners.clear(); api.deploymentsPage = originalDeployments; });
after(() => { closeChannels(); dom.window.close(); });

const row = { id: 1, name: "qwen-prod", model_name: "model", model_id: 1, served_name: "qwen", engine: "vllm", node_uuid: "node", desired_state: "active", runtime_state: "pulling", sleepable: false, replicas: 1, template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [] };
const settle = (ms: number) => act(() => new Promise<void>((resolve) => setTimeout(resolve, ms)));

test("the deployment list reloads for state changes and not for progress notices", async () => {
  let calls = 0;
  api.deploymentsPage = async () => { calls += 1; return listPage([row]) as never; };
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><DeploymentsPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("qwen-prod");
  const afterLoad = calls;

  for (let i = 0; i < 20; i++) {
    Events.emit("instance", "progress");
    Events.emit("modelfile", "progress");
  }
  await settle(1300);
  assert.equal(calls, afterLoad, "progress notices reloaded the whole list");

  Events.emit("instance", "running");
  await waitFor(() => assert.equal(calls, afterLoad + 1, "an instance state change did not reload the list"));

  Events.emit("modelfile", "present");
  await waitFor(() => assert.equal(calls, afterLoad + 2, "a model file state change did not reload the list"));

  Events.emit("node", "state:offline");
  await waitFor(() => assert.equal(calls, afterLoad + 3, "a node disconnect did not reload readiness"));
  Events.emit("node", "state:online");
  await waitFor(() => assert.equal(calls, afterLoad + 4, "a node reconnect did not reload readiness"));
});

test("the deployment list displays the instance node when scheduling chose a different node", async () => {
  api.deploymentsPage = async () => listPage([{ ...row, node_name: "target-node", runtime_node_names: ["actual-node"] }]) as never;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><DeploymentsPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("qwen-prod");
  assert.ok(screen.getByText("actual-node"));
  assert.equal(screen.queryByText("target-node"), null);
});
