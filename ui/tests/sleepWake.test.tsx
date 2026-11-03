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

const read = (file: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${file}`, import.meta.url), "utf8"));
const deployLocale = read("deploy.json");
const deploymentsLocale = read("deployments.json");
const commonLocale = read("common.json");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, deployments: deploymentsLocale, common: commonLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const SleepModeDialog = (await import("../src/components/SleepModeDialog.tsx")).default;
const EvictionSuggestionAlert = (await import("../src/pages/deploy/EvictionSuggestion.tsx")).default;
const { scheduleText } = await import("../src/pages/deploy/deploySchedule.ts");
const { APIError } = await import("../src/apiError.ts");
type Deployment = import("../src/api").Deployment;

afterEach(() => cleanup());

const wrap = (node: React.ReactNode) => <I18nextProvider i18n={i18n}>{node}</I18nextProvider>;
const named = (text: string) => new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));

test("the sleep mode dialog defaults to finishing in-flight requests and never offers keep", async () => {
  const chosen: string[] = [];
  render(wrap(<SleepModeDialog open level={1} onConfirm={(mode) => chosen.push(mode)} onCancel={() => {}} />));
  await screen.findByText(deploymentsLocale.sleepMode.titleQuick);
  const wait = screen.getByRole("radio", { name: named(deploymentsLocale.sleepMode.wait) }) as HTMLInputElement;
  const abort = screen.getByRole("radio", { name: named(deploymentsLocale.sleepMode.abort) }) as HTMLInputElement;
  assert.equal(wait.checked, true);
  assert.equal(abort.checked, false);
  assert.equal(screen.queryByText(/keep/i), null);
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  assert.deepEqual(chosen, ["wait"]);

  fireEvent.click(abort);
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  assert.deepEqual(chosen, ["wait", "abort"]);
});

test("given the server's default, the dialog asks whether a request wakes the sleeper and sends the choice", async () => {
  const chosen: Array<[string, boolean | undefined]> = [];
  const { rerender } = render(wrap(<SleepModeDialog open level={1} wakeOnRequest onConfirm={(mode, wakes) => chosen.push([mode, wakes])} onCancel={() => {}} />));
  const box = await screen.findByRole("checkbox", { name: named(deploymentsLocale.sleepMode.wakeOnRequest) }) as HTMLInputElement;
  assert.equal(box.checked, true, "the server's default");
  assert.ok(screen.getByText(deploymentsLocale.sleepMode.wakeOnRequestOn));
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  fireEvent.click(box);
  assert.ok(screen.getByText(deploymentsLocale.sleepMode.wakeOnRequestOff));
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  assert.deepEqual(chosen, [["wait", true], ["wait", false]]);
  // Opened again, it starts from the server's default.
  rerender(wrap(<SleepModeDialog open={false} level={1} wakeOnRequest onConfirm={(mode, wakes) => chosen.push([mode, wakes])} onCancel={() => {}} />));
  rerender(wrap(<SleepModeDialog open level={1} wakeOnRequest onConfirm={(mode, wakes) => chosen.push([mode, wakes])} onCancel={() => {}} />));
  await waitFor(() => assert.equal((screen.getByRole("checkbox", { name: named(deploymentsLocale.sleepMode.wakeOnRequest) }) as HTMLInputElement).checked, true));
});

test("a held default is unchecked, and without a default the dialog leaves the choice to the server", async () => {
  const chosen: Array<[string, boolean | undefined]> = [];
  const { unmount } = render(wrap(<SleepModeDialog open level={1} wakeOnRequest={false} onConfirm={(mode, wakes) => chosen.push([mode, wakes])} onCancel={() => {}} />));
  const box = await screen.findByRole("checkbox", { name: named(deploymentsLocale.sleepMode.wakeOnRequest) }) as HTMLInputElement;
  assert.equal(box.checked, false);
  assert.ok(screen.getByText(deploymentsLocale.sleepMode.wakeOnRequestOff));
  unmount();
  render(wrap(<SleepModeDialog open level={1} onConfirm={(mode, wakes) => chosen.push([mode, wakes])} onCancel={() => {}} />));
  await screen.findByText(deploymentsLocale.sleepMode.titleQuick);
  assert.equal(screen.queryByRole("checkbox"), null);
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  assert.deepEqual(chosen, [["wait", undefined]]);
});

test("the deep sleep title says nothing about disk", async () => {
  render(wrap(<SleepModeDialog open level={2} onConfirm={() => {}} onCancel={() => {}} />));
  await screen.findByText(deploymentsLocale.sleepMode.titleDeep);
  assert.doesNotMatch(deploymentsLocale.sleepMode.titleDeep, /disk/i);
  assert.doesNotMatch(deploymentsLocale.sleep.deep, /disk/i);
});

const suggestion = { instance_uuid: "inst-1", deployment_id: 7, deployment: "qwen3-0-6b", gpu_indexes: [0], frees_gib: 14.5 };
const sleepingDeployment = (level: number): Deployment => ({ id: 7, instances: [{ uuid: "inst-1", state: "running", sleep_level: level }] } as unknown as Deployment);

test("sleep-and-reevaluate orders the chosen mode, waits for the report, then reevaluates", async () => {
  const orders: unknown[][] = [];
  let polls = 0;
  let reevaluated = 0;
  render(wrap(
    <EvictionSuggestionAlert
      suggestion={suggestion}
      onSlept={() => { reevaluated++; }}
      sleeper={async (...args) => { orders.push(args); }}
      deploymentFetcher={async () => sleepingDeployment(++polls >= 2 ? 1 : 0)}
      pollMs={5}
    />,
  ));
  assert.ok(screen.getByText(/can free about 14\.5 GiB/));
  fireEvent.click(screen.getByRole("button", { name: deployLocale.eval.sleepAndReevaluate }));
  await screen.findByText(deploymentsLocale.sleepMode.titleQuick);
  fireEvent.click(screen.getByRole("radio", { name: named(deploymentsLocale.sleepMode.abort) }));
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  await waitFor(() => assert.equal(reevaluated, 1));
  assert.deepEqual(orders, [[7, 1, "abort"]]);
  assert.ok(polls >= 2, `polled ${polls} times`);
  // onSlept runs right after the "slept" state is set, before React commits
  // it; the text is on screen once that render lands.
  assert.ok(await screen.findByText(/is asleep; reevaluating/));
  assert.equal(screen.queryByRole("button", { name: deployLocale.eval.sleepAndReevaluate }), null);
});

test("a multi-node deployment is asleep once its leaders report it; followers report no level", async () => {
  let reevaluated = 0;
  const multiNode = { id: 7, instances: [
    { uuid: "inst-1", state: "running", sleep_level: 1, dist_role: "leader", nnodes: 2, node_rank: 0 },
    { uuid: "inst-1-f", state: "running", sleep_level: 0, dist_role: "follower", nnodes: 2, node_rank: 1, headless: true },
  ] } as unknown as Deployment;
  render(wrap(
    <EvictionSuggestionAlert
      suggestion={suggestion}
      onSlept={() => { reevaluated++; }}
      sleeper={async () => {}}
      deploymentFetcher={async () => multiNode}
      pollMs={5}
      timeoutMs={2000}
    />,
  ));
  fireEvent.click(screen.getByRole("button", { name: deployLocale.eval.sleepAndReevaluate }));
  await screen.findByText(deploymentsLocale.sleepMode.titleQuick);
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  await waitFor(() => assert.equal(reevaluated, 1));
});

test("a refused sleep order is shown by its stable code and leaves the button available", async () => {
  render(wrap(
    <EvictionSuggestionAlert
      suggestion={suggestion}
      sleeper={async () => { throw new APIError("SLEEP_ENGINE_UNSUPPORTED", { engine: "sglang" }, "the deployment's engine does not support sleep mode", 400); }}
      deploymentFetcher={async () => sleepingDeployment(0)}
      pollMs={5}
    />,
  ));
  fireEvent.click(screen.getByRole("button", { name: deployLocale.eval.sleepAndReevaluate }));
  await screen.findByText(deploymentsLocale.sleepMode.titleQuick);
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  await screen.findByText(/The sglang engine does not support sleep mode/);
  // The button's loading icon lingers through its leave animation in jsdom
  // and joins the accessible name, so match the label rather than the whole name.
  assert.ok(screen.getByRole("button", { name: named(deployLocale.eval.sleepAndReevaluate) }));
});

test("a sleep that never reports asleep times out with a pointer to the instance events", async () => {
  let reevaluated = 0;
  render(wrap(
    <EvictionSuggestionAlert
      suggestion={suggestion}
      onSlept={() => { reevaluated++; }}
      sleeper={async () => {}}
      deploymentFetcher={async () => sleepingDeployment(0)}
      pollMs={5}
      timeoutMs={40}
    />,
  ));
  fireEvent.click(screen.getByRole("button", { name: deployLocale.eval.sleepAndReevaluate }));
  await screen.findByText(deploymentsLocale.sleepMode.titleQuick);
  fireEvent.click(screen.getByRole("button", { name: deploymentsLocale.sleepMode.confirm }));
  await screen.findByText(/did not report sleep within/);
  assert.equal(reevaluated, 0);
});

test("a no_fit reason names the sleeping residue when the scheduler reported one", () => {
  const t = i18n.getFixedT("en", "deploy");
  const plain = scheduleText(t, { code: "no_fit", params: { tp: 1, gpu_count: 1, required_gib: "22.6", available_gib: "22.5", gpu_memory_utilization: 0.95 } });
  assert.match(plain, /22\.5 GiB available/);
  assert.doesNotMatch(plain, /resident/);
  const withResidue = scheduleText(t, { code: "no_fit", params: { tp: 1, gpu_count: 1, required_gib: "22.6", available_gib: "22.5", gpu_memory_utilization: 0.95, sleeping_gib: "1.5" } });
  assert.match(withResidue, /22\.5 GiB available/);
  assert.match(withResidue, /keep 1\.5 GiB resident/);
});

test("a startup reservation blocked by sleep residue explains the contradictory capacity", () => {
  const t = i18n.getFixedT("en", "deploy");
  const text = scheduleText(t, {
    code: "sleep_residue_blocks_startup",
    params: { startup_gib: "22.8", available_gib: "22.5", sleeping_gib: "1.5" },
  });
  assert.match(text, /startup reservation is 22\.8 GiB/);
  assert.match(text, /only 22\.5 GiB is free/);
  assert.doesNotMatch(text, /keep 1\.5 GiB resident/);
});

test("the wake refusal text carries the GPU, the shortfall and the occupant", () => {
  const t = i18n.getFixedT("en", "common");
  const text = t("apiError.WAKE_INSUFFICIENT_GPU_MEMORY", { node: "gpu3", gpu_indexes: [0], shortfall_gib: "21.6", occupants: ["newcomer"] });
  assert.match(text, /GPU 0 on gpu3/);
  assert.match(text, /21\.6 GiB short/);
  assert.match(text, /Stop or sleep newcomer first/);
});
