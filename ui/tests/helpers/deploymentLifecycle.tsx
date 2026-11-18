import { installCanvas } from "./analyticsFixtures.ts";
import assert from "node:assert/strict";
import { fakeFleet } from "./fakeFleet.ts";
import { listPage, cursorPage, list } from "./listPage.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// The deployment drawer's lifecycle, for every shape: the header's Start
// (stopped deployments), Restart (a confirm, not rolling) and Stop (keeps the
// drawer open); an ordinary deployment's replica that gave up with its
// Retry; the leader/follower rank tags of a multi-node replica across pages; and the monitor's instance link scrolling to its panel.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement", "HTMLHtmlElement", "NodeFilter", "HTMLInputElement", "HTMLButtonElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
const scrolled: string[] = [];
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
  scrollIntoView: { configurable: true, value(this: HTMLElement) { scrolled.push(this.id); } },
});

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    deploy: JSON.parse(fs.readFileSync(new URL("../../src/i18n/locales/en/deploy.json", import.meta.url), "utf8")),
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});
const locale = i18n.getResourceBundle("en", "deployments");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;

class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const { selectInstance } = await import("./selectInstance.ts");
const { api } = await import("../../src/api.ts");
const { APIError } = await import("../../src/apiError.ts");
const { default: DeploymentsPage } = await import("../../src/pages/DeploymentsPage.tsx");

const apiAny = api as unknown as Record<string, unknown>;
function stub(name: string, value: unknown) { apiAny[name] = value; }

// stubFleet answers the node endpoints as the server does (one searched page,
// single nodes, facets) and refuses listing every node.
function stubFleet(list: unknown[]) {
  const fake = fakeFleet(list as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fake[name]);
  return fake;
}
function resetStubs() {
  stub("routingPolicies", async () => list([]));
  stub("instanceEvents", async () => cursorPage([]));
  stub("metricsQuery", async () => list([]));
  stubFleet([]);
  stub("monitorDeployment", async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  }));
  stub("deploymentEvents", async () => cursorPage([]));
}
resetStubs();
afterEach(() => { cleanup(); Events.listeners.clear(); resetStubs(); scrolled.length = 0; });
after(() => { closeChannels(); dom.window.close(); });

// instance is an instance as the server lists it, with the server's
// verdicts: replica names the replica it fills (none for a failed attempt),
// and it takes no sleep order of its own (the deployments here cannot sleep).
function instance(uuid: string, extra: Record<string, unknown> = {}): unknown {
  return {
    uuid, deployment_id: 1, node_uuid: `node-${uuid}`, state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "runner:vllm",
    message: "", container_id: `container-${uuid}`, sleep_level: 0, desired_sleep: 0, dist_role: "leader", nnodes: 1, node_rank: 0,
    asleep: false, can_sleep_alone: false, can_wake_alone: false,
    ...extra,
  };
}

function deployment(overrides: Record<string, unknown> = {}) {
  return {
    id: 1, name: "lifecycle-deployment", model_name: "model", model_id: 1, served_name: "model",
    engine: "vllm", node_uuid: "node-a", desired_state: "active", runtime_state: "running",
    sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 2, max_replicas: 32, template_deviated: false, created_at: "2026-09-30T00:00:00Z",
    operations: [], instances: [instance("inst-a", { replica: { index: 0 } }), instance("inst-b", { replica: { index: 1 } })],
    ...overrides,
  };
}

async function openDetail(search = "?deployment=1") {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[`/deployments${search}`]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  await screen.findAllByText("lifecycle-deployment");
  return screen.findByRole("button", { name: locale.detail.stop });
}

function drawer(): HTMLElement {
  return screen.getByRole("dialog", { name: "lifecycle-deployment" });
}


const replicaNodes = [
  { uuid: "node-a", name: "Node A", hostname: "a", state: "online", gpu_count: 2, gpus: [] },
  { uuid: "node-b", name: "Node B", hostname: "b", state: "online", gpu_count: 2, gpus: [] },
];

function multiNodeDeployment(overrides: Record<string, unknown> = {}) {
  return deployment({
    nnodes: 2, replicas: 1,
    units: [{ node_uuid: "node-a", node_name: "Node A", followers: [{ node_uuid: "node-b", node_name: "Node B" }] }],
    instances: [
      instance("lead-a", { node_uuid: "node-a", nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g1", dist_attempt: 1, replica: { index: 0 } }),
      instance("follow-b", { node_uuid: "node-b", nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g1", dist_attempt: 1, replica: { index: 0 } }),
    ],
    ...overrides,
  });
}

// chooseOption opens a select and picks an option, waiting for it: a node
// picker's options arrive from a server search after it opens.
async function chooseOption(select: HTMLElement, label: string) {
  const selector = select.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement;
  fireEvent.mouseDown(selector);
  let option: Element | undefined;
  await waitFor(() => {
    const dropdowns = document.querySelectorAll(".ant-select-dropdown");
    option = [...dropdowns[dropdowns.length - 1].querySelectorAll(".ant-select-item-option")].find((el) => el.textContent === label);
    if (!option) throw new Error(`option ${label} not offered`);
  });
  fireEvent.click(option!);
}

export { assert, test, React, I18nextProvider, MemoryRouter, render, screen, waitFor, within, cleanup, fireEvent, userEvent, list, listPage, cursorPage, dom, i18n, locale, Events, scrolled, api, APIError, DeploymentsPage, selectInstance, stub, stubFleet, resetStubs, instance, deployment, openDetail, drawer, multiNodeDeployment, replicaNodes, chooseOption };
