import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { installCanvas } from "./helpers/analyticsFixtures.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { cursorPage } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class EventSourceStub { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    networks: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/networks.json", import.meta.url), "utf8")),
    deploy: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8")),
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstancePanel } = await import("../src/pages/InstancePanel.tsx");
const { lastWakeSeconds } = await import("../src/pages/instanceWake.ts");
const originalFetch = globalThis.fetch;
const originalInstanceEvents = api.instanceEvents;
const originalDeploymentEvents = api.deploymentEvents;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  api.instanceEvents = originalInstanceEvents;
  api.deploymentEvents = originalDeploymentEvents;
});

const base = {
  uuid: "launch-facts-ui", deployment_id: 1, node_uuid: "node-1", state: "running",
  gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image", message: "", container_id: "abc123", sleep_level: 0,
} as never;

function renderPanel(inst: object = base, sleepable = false) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst as never} sleepable={sleepable} /></MemoryRouter></I18nextProvider>);
}

function response(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

test("a whole-card instance shows a scheduling exception only when the selected GPUs differ from its assignment", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const inst = { ...(base as object), gpu_indexes: [1, 0], schedule: { explain: { topo_highlight: { gpus: [0, 1] } } }, env: { MODE: "local" } };
  const { unmount } = renderPanel(inst);
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts" }));
  assert.ok(screen.getByText("MODE=local"));
  assert.equal(screen.queryByText(/node adds dGPU/), null);
  assert.equal(screen.queryByText("Scheduled GPUs"), null);
  unmount();

  renderPanel({ ...inst, gpu_indexes: [0], vram_limit_bytes: 8 * 1024 ** 3 });
  assert.ok(screen.getByText("Scheduled GPUs"));
  assert.ok(screen.getByText("0, 1"));
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts" }));
  assert.ok(screen.getByText(/node adds dGPU/));
});

// The ticket's instance: Qwen3-0.6B (about 1.4 GiB of weights) on a 24 GiB
// card under an 8 GiB dGPU limit, whose wakes took 25 s and more while the
// panel printed a 0.1 s estimate worked out from the weight size alone.
const smallModel = {
  ...(base as object), uuid: "wake-a", deployment_id: 1641, vram_limit_bytes: 8 * 1024 ** 3,
  schedule: { breakdown: { weight_bytes: 1.5e9, tensor_parallel: 1 } },
};
const sleepResult = (id: number, at: string, level: number) =>
  ({ id, kind: "sleep", code: "INSTANCE_SLEEP_RESULT", params: { result: "applied", level }, created_at: at });

test("the sleep row reports the wake measured on the instance and no figure before one is recorded", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const asked: number[] = [];
  let events: object[] = [];
  api.deploymentEvents = (async (id: number) => { asked.push(id); return cursorPage(events as never[]); }) as typeof api.deploymentEvents;

  // Not sleepable: the timeline is not read at all.
  const plain = renderPanel(smallModel);
  assert.equal(screen.queryByText("Sleep"), null);
  assert.deepEqual(asked, []);
  plain.unmount();

  // Sleepable, never woken: the weight size alone yields no wake figure.
  const fresh = renderPanel(smallModel, true);
  await waitFor(() => assert.deepEqual(asked, [1641]));
  assert.equal(screen.queryByText(/wake took|wake time|0\.1 s/), null);
  fresh.unmount();

  // A request woke it at 17:52:10 and the engine reported awake 25.4 s later.
  events = [
    sleepResult(14, "2026-11-19T09:52:35.400Z", 0),
    { id: 13, kind: "automation", code: "AUTO_WAKE", params: { trigger: "request", instances: ["wake-a"] }, created_at: "2026-11-19T09:52:10Z" },
    sleepResult(12, "2026-11-19T09:51:00Z", 1),
    { id: 11, kind: "lifecycle", code: "SLEEP_ORDERED", params: { level: 1, instances: ["wake-a"] }, created_at: "2026-11-19T09:50:58Z" },
  ];
  const awake = renderPanel(smallModel, true);
  assert.ok(await screen.findByText("Last wake took about 25 s"));
  awake.unmount();

  // Asleep again at L2: the same measurement stands beside the level.
  renderPanel({ ...smallModel, sleep_level: 2, desired_sleep: 2, asleep: true }, true);
  assert.ok(await screen.findByText("Last wake took about 25 s"));
  assert.ok(screen.getByText(i18n.t("instance.sleepWeight", { storage: i18n.t("instance.disk"), gib: (1.5e9 / 1024 ** 3).toFixed(1) })));
});

test("a wake is measured from the order that named the instance to its own awake report", () => {
  const order = (id: number, at: string, code: string, instances: string[]) => ({ id, kind: "automation", code, params: { instances }, created_at: at });
  const at = (seconds: number) => new Date(Date.UTC(2026, 10, 19, 9, 0, seconds)).toISOString();

  // An operator's wake of one replica, 1.7 s.
  assert.equal(lastWakeSeconds([sleepResult(3, "2026-11-19T09:00:01.700Z", 0), order(2, at(0), "WAKE_ORDERED", ["a"])] as never[], [], "a"), 1.7);
  // No order on record, an order for another replica, or no report yet.
  assert.equal(lastWakeSeconds([sleepResult(3, at(2), 0)] as never[], [], "a"), undefined);
  assert.equal(lastWakeSeconds([sleepResult(3, at(2), 0), order(2, at(0), "AUTO_WAKE", ["b"])] as never[], [], "a"), undefined);
  assert.equal(lastWakeSeconds([order(2, at(0), "AUTO_WAKE", ["a"])] as never[], [], "a"), undefined);
  // A sleep reported after the order is not the wake's result.
  assert.equal(lastWakeSeconds([sleepResult(3, at(2), 1), order(2, at(0), "AUTO_WAKE", ["a"])] as never[], [], "a"), undefined);
  // The failed wake's rollback is not a wake either.
  const failed = { id: 3, kind: "sleep", code: "INSTANCE_SLEEP_RESULT", params: { result: "failed_rollback", target: 0, actual: 1 }, created_at: at(2) };
  assert.equal(lastWakeSeconds([failed, order(2, at(0), "AUTO_WAKE", ["a"])] as never[], [], "a"), undefined);

  // Two replicas woken by one order: each one's report is told apart by its
  // own timeline, and without that neither is claimed.
  const both = [sleepResult(5, at(30), 0), sleepResult(4, at(3), 0), order(2, at(0), "AUTO_WAKE", ["a", "b"])] as never[];
  assert.equal(lastWakeSeconds(both, [sleepResult(5, at(30), 0)] as never[], "a"), 30);
  assert.equal(lastWakeSeconds(both, [sleepResult(4, at(3), 0)] as never[], "b"), 3);
  assert.equal(lastWakeSeconds(both, [], "a"), undefined);

  // A report that follows a later order may answer that one.
  assert.equal(lastWakeSeconds([sleepResult(5, at(9), 0), order(4, at(8), "WAKE_ORDERED", ["b"]), order(2, at(0), "AUTO_WAKE", ["a"])] as never[], [], "a"), undefined);
  // Only the latest wake counts.
  assert.equal(lastWakeSeconds([sleepResult(9, at(26), 0), order(8, at(0), "AUTO_WAKE", ["a"]), sleepResult(3, "2026-11-19T08:00:02Z", 0), order(2, "2026-11-19T08:00:00Z", "AUTO_WAKE", ["a"])] as never[], [], "a"), 26);
});

test("launch facts keeps loading and transport error states distinct", async () => {
  api.instanceEvents = async () => cursorPage([]);
  let resolve: ((value: Response) => void) | undefined;
  globalThis.fetch = (() => new Promise<Response>((done) => { resolve = done; })) as typeof fetch;
  renderPanel();
  fireEvent.click(screen.getByRole("button", { name: i18n.t("instance.launchFacts.title"), expanded: false }));
  assert.ok(screen.getByLabelText("Loading launch facts"));
  resolve?.(response({}, 503));
  await waitFor(() => assert.ok(screen.getByText("Launch facts could not be loaded.")));
  assert.ok(screen.getByRole("button", { name: "Retry" }));
});

test("launch facts renders unknown reasons and current relation from the server projection", async () => {
  api.instanceEvents = async () => cursorPage([]);
  const facts = {
    intent: { status: "unknown" }, resolved: { status: "unknown" },
    runtime_observation_relation: { status: "current" },
    runtime_observation_received_at: "2026-09-20T00:00:00Z",
    runtime_observation: {
      runtime: "docker",
      container: { status: "observed", container_id: "abc123", image_id: "sha256:image", entrypoint: [], cmd: [], args: [], env: {}, mounts: [], devices: [] },
      image: { status: "unknown", reason: "not_collected", entrypoint: [], cmd: [], env: {}, volumes: [] },
      process: { status: "unknown", reason: "non_local_pid" },
    },
  };
  globalThis.fetch = (async (input) => {
    const url = String(input);
    return url.includes("/metrics") ? response([]) : response(facts);
  }) as typeof fetch;
  renderPanel();
  fireEvent.click(screen.getByRole("button", { name: i18n.t("instance.launchFacts.title"), expanded: false }));
  await waitFor(() => assert.ok(screen.getByText("Observation relation:")));
  assert.ok(screen.getByText("Current"));
  screen.getByText("Observed image").click();
  await waitFor(() => assert.ok(screen.getByText("This layer has not been collected.")));
  screen.getByText("Observed process").click();
  await waitFor(() => assert.ok(screen.getByText("The process was not proven to be local to this runtime.")));
});

test("a multi-node rank shows the compute network it started on", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const management = { name: "management", management: true, type: "host_ethernet", interface: "eth0", ip: "172.25.0.11" };
  const { unmount } = renderPanel({ ...(base as object), schedule: { network: { compute: management } } });
  assert.ok(screen.getByText("Compute network"));
  assert.ok(screen.getByRole("link", { name: "Management network" }));
  assert.ok(screen.getByText("Fallback: the group shares no dedicated network"));
  assert.ok(screen.getByText("eth0 · 172.25.0.11"));
  unmount();

  const rdma = { name: "ib-a", management: false, type: "host_rdma", interface: "bond0", ip: "10.20.1.11", link_layer: "infiniband" };
  renderPanel({ ...(base as object), schedule: { network: { compute: rdma } } });
  assert.ok(screen.getByText("ib-a"));
  assert.ok(screen.getByText("InfiniBand"));
  assert.equal(screen.queryByText("Fallback: the group shares no dedicated network"), null);
});

test("a single-node instance has no compute network row", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  renderPanel();
  assert.equal(screen.queryByText("Compute network"), null);
});

test("the runtime section states the /dev/shm size, or the host's under --ipc=host", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const { unmount } = renderPanel({ ...(base as object), shm_bytes: 32 * (1 << 30), docker_args: ["--ulimit", "nofile=65536:65536"] });
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts", expanded: false }));
  await waitFor(() => assert.ok(screen.getByText("/dev/shm 32 GiB")));
  assert.equal(screen.queryByText("Host /dev/shm (--ipc=host)"), null);
  unmount();

  renderPanel({ ...(base as object), docker_args: ["--ipc", "host"] });
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts", expanded: false }));
  await waitFor(() => assert.ok(screen.getByText("Host /dev/shm (--ipc=host)")));
  assert.equal(screen.queryByText(/^\/dev\/shm \d/), null);
});

test("the panel names the model files the instance reads and the storage each is in", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response({})) as typeof fetch;
  renderPanel({
    ...(base as object),
    model_files: [
      { model_id: 7, storage_id: 2, storage_name: "lab-nfs", storage_type: "nfs", path: "/mnt/gpuworks/lab-nfs/Qwen/Qwen2.5-0.5B" },
      { model_id: 8, path: "/opt/gpuworks/models/draft" },
    ],
  });
  assert.ok(screen.getByText("Model files"));
  assert.ok(screen.getByRole("link", { name: "lab-nfs" }));
  assert.ok(screen.getByText("/mnt/gpuworks/lab-nfs/Qwen/Qwen2.5-0.5B"));
  // A file whose storage was removed since still shows where it was read.
  assert.ok(screen.getByText("Storage removed"));
  assert.ok(screen.getByText("/opt/gpuworks/models/draft"));
});

test("an instance from before model files were recorded shows no model files row", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response({})) as typeof fetch;
  renderPanel();
  assert.equal(screen.queryByText("Model files"), null);
});

// The allocation card is the engine's startup log (weights, KV cache, CUDA
// graphs), not what nvidia-smi reports per process, so an engine that logs
// none of it leaves the card without figures on any hardware, unified-memory
// boards included. It says why and where the card-level memory use is.
test("an instance whose engine log has no memory figures explains the empty allocation card", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const breakdown = { weight_bytes: 1.5e9, per_gpu_bytes: 2e9, kv_pool_bytes: 1e9, tensor_parallel: 1, grade: "rough" };
  const silent = renderPanel({ ...(base as object), uuid: "no-memory-log", schedule: { breakdown }, measured: { Found: false } });
  assert.ok(screen.getByText(/No GPU memory breakdown \(weights, KV cache, CUDA graphs\) was parsed from the engine's startup log yet/));
  assert.ok(screen.getByText(/overall GPU memory use is under "GPU & memory"/));
  silent.unmount();

  renderPanel({
    ...(base as object), uuid: "with-memory-log", schedule: { breakdown },
    measured: { Found: true, ModelWeightsGiB: 1.4, KVCacheGiB: 6, KVCacheTokens: 50000, GraphCaptureGiB: 0.5, MaxConcurrency: 4, NumGPUBlocks: 100 },
  });
  assert.equal(screen.queryByText(/No GPU memory breakdown/), null);
  assert.ok(screen.getByText("KV pool 6.0 GiB · 50,000 tokens reported by the engine at startup"));
});
