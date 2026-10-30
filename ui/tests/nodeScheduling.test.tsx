import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// A node is cordoned, drained or made schedulable again from its scheduling
// dialog, which lists what still runs there and whether a drain moves it,
// warns about replicas a drain leaves, and evicts on the operator's word.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", resources: { en: { nodes: read("nodes"), common: read("common"), deploy: read("deploy") } },
  defaultNS: "nodes", interpolation: { escapeValue: false },
});
const sched = i18n.getResourceBundle("en", "nodes").scheduling;

const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { NodeSchedulingModal, NodeSchedulingTag } = await import("../src/components/NodeSchedulingModal.tsx");

const apiAny = api as unknown as Record<string, unknown>;
const saved = { nodeReplicas: apiAny.nodeReplicas, setNodeScheduling: apiAny.setNodeScheduling, evictNode: apiAny.evictNode };
afterEach(() => { cleanup(); Object.assign(apiAny, saved); });
after(() => { closeChannels(); dom.window.close(); });

const node = { uuid: "n-1", name: "gpu-1", state: "online", scheduling: "schedulable" } as never;
const items = [
  { instance: "pool-instance-1", deployment_id: 1, deployment: "chat", state: "running", kind: "pool" },
  { instance: "pinned-instance", deployment_id: 2, deployment: "pinned", state: "running", kind: "pinned" },
  { instance: "rank-instance-1", deployment_id: 3, deployment: "big", state: "running", kind: "named" },
];

function show(onChanged: (n: unknown) => void = () => {}) {
  return render(<I18nextProvider i18n={i18n}><NodeSchedulingModal node={node} onClose={() => {}} onChanged={onChanged} /></I18nextProvider>);
}

test("only a node that takes no new replicas is tagged", () => {
  render(<I18nextProvider i18n={i18n}><NodeSchedulingTag node={node} /><NodeSchedulingTag node={{ ...(node as object), scheduling: "draining" } as never} /></I18nextProvider>);
  assert.equal(screen.queryByText(sched.state.schedulable), null);
  assert.ok(screen.getByText(sched.state.draining));
});

// A cordon the platform applied is explained where the tag is, not only
// in the dialog behind it.
test("a node cordoned because its runtime cannot use the cards says why on hover", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const blocked = { ...(node as object), scheduling: "cordoned", runtime_driver: "docker", runtime_gpu_blocked: "gpu_plugin_missing", runtime_gpu_plugin: "NVIDIA Container Toolkit" } as never;
  render(<I18nextProvider i18n={i18n}><NodeSchedulingTag node={blocked} /></I18nextProvider>);
  await user.hover(screen.getByText(sched.state.cordoned));
  const tip = await screen.findByRole("tooltip");
  assert.ok(tip.textContent?.includes("NVIDIA Container Toolkit"), tip.textContent ?? "");
  assert.ok(tip.textContent?.includes("docker"), tip.textContent ?? "");
  assert.ok(!tip.textContent?.includes("gpu_plugin_missing"), "the raw code is not shown");
});

test("a cordon the operator chose has no tooltip, and a cause the client does not know shows as its code", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><NodeSchedulingTag node={{ ...(node as object), scheduling: "cordoned" } as never} /></I18nextProvider>);
  await user.hover(screen.getByText(sched.state.cordoned));
  assert.equal(screen.queryByRole("tooltip"), null, "nothing to explain");
  cleanup();
  render(<I18nextProvider i18n={i18n}><NodeSchedulingTag node={{ ...(node as object), scheduling: "cordoned", runtime_gpu_blocked: "gpu_from_the_future" } as never} /></I18nextProvider>);
  await user.hover(screen.getByText(sched.state.cordoned));
  assert.ok((await screen.findByRole("tooltip")).textContent?.includes("gpu_from_the_future"));
});

test("draining lists what a drain moves and warns about what it leaves; evicting asks first", async () => {
  let scheduling = "schedulable";
  const set: string[] = [];
  let evicted = 0;
  apiAny.nodeReplicas = async () => ({ scheduling, items });
  apiAny.setNodeScheduling = async (uuid: string, next: string) => {
    assert.equal(uuid, "n-1");
    set.push(next);
    scheduling = next;
    return { ...(node as object), scheduling: next };
  };
  apiAny.evictNode = async () => { evicted++; return { evicted: ["pool-instance-1", "pinned-instance", "rank-instance-1"] }; };
  const changed: unknown[] = [];
  const user = userEvent.setup({ document: dom.window.document });
  show((n) => changed.push(n));
  await screen.findByText("chat");
  assert.ok(screen.getByText(sched.kind.pool) && screen.getByText(sched.kind.pinned) && screen.getByText(sched.kind.named));
  assert.equal(screen.queryByRole("button", { name: sched.action.evict }), null, "a schedulable node cannot be evicted");

  await user.click(screen.getByRole("button", { name: sched.action.drain }));
  await screen.findByText(i18n.t("scheduling.stuck", { count: 1 }));
  assert.deepEqual(set, ["draining"]);
  assert.equal(changed.length, 1);

  await user.click(screen.getByRole("button", { name: sched.action.evict }));
  const confirm = await screen.findByText(sched.action.evictConfirm);
  const popover = confirm.closest(".ant-popover") as HTMLElement;
  await user.click(within(popover).getByRole("button", { name: sched.action.evict }));
  await waitFor(() => assert.equal(evicted, 1));

  // The buttons show a spinner while the eviction's reload runs; jsdom never
  // finishes the spinner's leave motion, so its label stays in the name.
  await waitFor(() => assert.equal(document.querySelectorAll(".ant-btn-loading").length, 0));
  await user.click(screen.getByRole("button", { name: new RegExp(`${sched.action.uncordon}$`) }));
  await waitFor(() => assert.deepEqual(set, ["draining", "schedulable"]));
});

test("loading nothing, and a refused change, say so", async () => {
  apiAny.nodeReplicas = async () => ({ scheduling: "cordoned", items: [] });
  apiAny.setNodeScheduling = async () => { throw new APIError("FIELD_INVALID", { field: "scheduling", detail: "x" }, "bad", 400); };
  const user = userEvent.setup({ document: dom.window.document });
  show();
  await screen.findByText(sched.empty);
  assert.equal(screen.queryByRole("button", { name: sched.action.evict }), null, "nothing to evict");
  await user.click(screen.getByRole("button", { name: sched.action.drain }));
  await waitFor(() => assert.ok(document.querySelector(".ant-alert-error")));
});

// A node the platform cordoned because its runtime cannot give a container
// its accelerators: un-cordoning it asks first, and the warning names the
// cause in the operator's own language rather than echoing the code.
test("un-cordoning a node whose runtime cannot use its cards warns with the reason", async () => {
  for (const tc of [
    { code: "gpu_plugin_missing", plugin: "", names: ["NVIDIA Container Toolkit", "docker"] },
    { code: "gpu_runtime_unconfigured", plugin: "NVIDIA Container Toolkit 1.17.3", names: ["nvidia-ctk runtime configure", "docker"] },
  ]) {
    apiAny.nodeReplicas = async () => ({ scheduling: "cordoned", items: [] });
    const set: string[] = [];
    apiAny.setNodeScheduling = async (_uuid: string, next: string) => { set.push(next); return { ...(node as object), scheduling: next }; };
    const blocked = { ...(node as object), scheduling: "cordoned", runtime_driver: "docker", runtime_gpu_blocked: tc.code, runtime_gpu_plugin: "NVIDIA Container Toolkit", runtime_plugin: tc.plugin } as never;
    const user = userEvent.setup({ document: dom.window.document });
    render(<I18nextProvider i18n={i18n}><NodeSchedulingModal node={blocked} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);

    await user.click(await screen.findByRole("button", { name: new RegExp(`${sched.action.uncordon}$`) }));
    const title = await screen.findByText(sched.blocked.title);
    const popover = title.closest(".ant-popover") as HTMLElement;
    const warning = popover.textContent ?? "";
    for (const name of tc.names) {
      assert.ok(warning.includes(name), `${tc.code} warning does not name ${name}: ${warning}`);
    }
    assert.ok(!warning.includes(tc.code), `${tc.code} warning shows the raw code: ${warning}`);
    assert.deepEqual(set, [], "the node was un-cordoned before the operator confirmed");

    await user.click(within(popover).getByRole("button", { name: sched.action.uncordonAnyway }));
    await waitFor(() => assert.deepEqual(set, ["schedulable"]));
    cleanup();
  }
});

// An unknown code from a newer agent still reaches the operator: the code
// itself beats an empty warning.
test("a reason this client does not know is shown as the code", async () => {
  apiAny.nodeReplicas = async () => ({ scheduling: "cordoned", items: [] });
  apiAny.setNodeScheduling = async () => ({ ...(node as object), scheduling: "schedulable" });
  const blocked = { ...(node as object), scheduling: "cordoned", runtime_driver: "docker", runtime_gpu_blocked: "gpu_from_the_future" } as never;
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><NodeSchedulingModal node={blocked} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);
  await user.click(await screen.findByRole("button", { name: new RegExp(`${sched.action.uncordon}$`) }));
  const title = await screen.findByText(sched.blocked.title);
  assert.ok((title.closest(".ant-popover") as HTMLElement).textContent?.includes("gpu_from_the_future"));
});

test("a waiting pool drain exposes scheduler feedback and stops without enabling scheduling", async () => {
  let scheduling = "draining";
  const set: string[] = [];
  apiAny.nodeReplicas = async () => ({ scheduling, items: [{ ...items[0], drain_reason: { code: "no_free_gpu", message: "No free GPU on replacement-node" }, drain_message: "replacement image pull was denied" }] });
  apiAny.setNodeScheduling = async (_uuid: string, next: string) => { set.push(next); scheduling = next; return { ...(node as object), scheduling: next }; };
  const user = userEvent.setup({ document: dom.window.document });
  show();
  await screen.findByText("chat");
  assert.ok(await screen.findByText(/replacement image pull was denied/));
  assert.ok(screen.getByText(/Waiting for.*replacement/i));
  screen.getByRole("button", { name: "Stop drain" }).focus();
  await user.keyboard("{Enter}");
  await screen.findByText(sched.state.cordoned);
  assert.deepEqual(set, ["cordoned"], "stopping a drain keeps maintenance protection");
});


test("offline removal warning refreshes and clears after agent reconnection", async () => {
  let offline = true;
  let refresh: (() => void) | undefined;
  const originalInterval = window.setInterval;
  window.setInterval = ((callback: () => void) => { refresh = callback; return 12345; }) as typeof window.setInterval;
  try {
    apiAny.nodeReplicas = async () => ({ scheduling: "draining", drain_offline: offline, items: [{ ...items[0], state: "removing" }] });
    show();
    await screen.findByText(sched.offline);
    assert.ok(screen.getByText(sched.offlineHint));
    assert.ok(screen.getByRole("button", { name: "Stop drain" }));
    offline = false;
    await act(async () => { refresh?.(); });
    await waitFor(() => assert.equal(screen.queryByText(sched.offline), null));
    assert.ok(screen.getByText("chat"), "removal still awaits confirmation");
  } finally {
    cleanup();
    window.setInterval = originalInterval;
  }
});
