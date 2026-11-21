import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// How a carved card reads on the GPU page. The answers are the server's own
// (ui/tests/fixtures/gpu-inventory, generated from the real handlers): a
// whole card, a card carved into three slices with one still starting, and a
// card passed through to a virtual machine.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "KeyboardEvent", "Document", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
// The detail drawer mounts its metric charts; a canvas that answers every
// call keeps them alive, as the node-page tests do.
const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const gpus = read("gpus");
const common = read("common");
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { gpus, common } }, defaultNS: "gpus", interpolation: { escapeValue: false } });

const fixture = (name: string) => JSON.parse(fs.readFileSync(new URL(`./fixtures/gpu-inventory/${name}.json`, import.meta.url), "utf8"));
const gpusPage = fixture("gpus-page");
const dgpus = fixture("dgpus");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: GPUPage } = await import("../src/pages/GPUPage.tsx");
const { DGPUHistory, movingAverage } = await import("../src/pages/gpus/GPUDetailDrawers.tsx");
const { default: GPUMemoryComposition, memorySegments } = await import("../src/components/GPUMemoryComposition.tsx");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { gpuModelSummary } = await import("../src/nodes/cardIdentity.ts");

const originalFetch = globalThis.fetch;
globalThis.fetch = async (input) => {
  const url = String(input);
  if (url.startsWith("/api/v1/gpus?")) return Response.json(gpusPage);
  if (url.startsWith("/api/v1/dgpus")) return Response.json(dgpus);
  if (url.startsWith("/api/v1/metrics/query")) return Response.json({ items: [], total: 0 });
  throw new Error(`Unexpected request: ${url}`);
};
afterEach(() => { cleanup(); localStorage.clear(); });
after(() => { globalThis.fetch = originalFetch; closeChannels(); dom.window.close(); });

const GIB = 1024 ** 3;

function renderPage() {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/gpus"]}><GPUPage /></MemoryRouter></I18nextProvider>);
}

async function rowOf(model: string) {
  const cell = await screen.findByText(model);
  return cell.closest("tr") as HTMLElement;
}

test("a carved card says how much is carved; a card that takes no work offers no dGPU", async () => {
  renderPage();
  const carved = await rowOf("GeForce RTX 2080 Ti");
  // 8 + 8 + 4 GiB of limits on a 22 GiB card; the starting slice holds its
  // quota and is counted.
  assert.ok(within(carved).getByText("3 slices · 20 GiB / 22 GiB"));
  const passedThrough = await rowOf("GeForce RTX 3090");
  assert.ok(!within(passedThrough).queryByText(common.partitionCapability.available), "a passed-through card is not offered as dGPU-ready");
  const whole = await rowOf("Quadro RTX 8000");
  assert.ok(within(whole).getByText(common.partitionCapability.available), "an idle-able whole card still shows it can be carved");
});

test("the card's detail draws its memory as booked, slice by slice", async () => {
  renderPage();
  fireEvent.click(within(await rowOf("GeForce RTX 2080 Ti")).getByRole("button", { name: "gpu3" }));
  const drawer = await screen.findByRole("dialog");
  const legend = await within(drawer).findByText(gpus.composition.title);
  const section = legend.closest("section") as HTMLElement;
  const items = [...section.querySelectorAll(".gpu-memory-composition-legend li")].map((li) => li.textContent);
  assert.deepEqual(items, [
    "qwen3-dgpu-a · limit 8.0 GiB8.0 GiB",
    "qwen3-dgpu-b · limit 8.0 GiB8.0 GiB",
    "qwen3-dgpu-c · limit 4.0 GiB4.0 GiB",
    `${gpus.composition.overhead}1.5 GiB`,
    `${gpus.composition.free}0.5 GiB`,
    `${gpus.composition.usedLegend}14.7 GiB`,
  ]);
  // How the bar is drawn is behind the title's help, not a grey line under
  // the legend: the legend already names the marker.
  assert.equal(section.querySelector(".gpu-memory-composition-note"), null);
  assert.ok(!section.textContent?.includes(gpus.composition.titleHelp));
  const help = within(section).getByRole("button", { name: common.helpTip.label });
  help.focus();
  assert.ok(await screen.findByText(gpus.composition.titleHelp));
});

test("the dGPU list shows a starting slice with its state", async () => {
  renderPage();
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  const starting = (await screen.findByText("qwen3-dgpu-c")).closest("tr") as HTMLElement;
  assert.ok(within(starting).getByText(common.states.starting));
  const running = screen.getByText("qwen3-dgpu-a").closest("tr") as HTMLElement;
  assert.ok(within(running).getByText(common.states.running));
  assert.ok(within(running).getByText("7.0 GiB"));
});

test("a slice's detail shows its use only when its backend reports it", async () => {
  renderPage();
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  fireEvent.click(await screen.findByRole("button", { name: "qwen3-dgpu-a" }));
  const reported = await screen.findByRole("dialog");
  assert.ok(await within(reported).findByRole("heading", { name: gpus.detail.currentUse }));
  assert.ok(within(reported).getByText("7.0 GiB / 8.0 GiB"));
  assert.ok(within(reported).getByRole("heading", { name: gpus.detail.monitoringData }));
  cleanup();

  // A starting slice has reported no use yet: no empty block stands in for it.
  renderPage();
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  fireEvent.click(await screen.findByRole("button", { name: "qwen3-dgpu-c" }));
  const silent = await screen.findByRole("dialog");
  assert.ok(await within(silent).findByRole("heading", { name: gpus.detail.quotaInfo }));
  assert.equal(within(silent).queryByRole("heading", { name: gpus.detail.currentUse }), null);
  assert.equal(within(silent).queryByRole("heading", { name: gpus.detail.monitoringData }), null);
});

// GWKS-424: an Ascend or PPU slice now reports its memory like an NVIDIA one,
// and a reading its backend never keeps says so rather than "No data", which
// reads as a report that is late. The rows carry the keys GET /dgpus puts on
// the wire for each case (TestDGPUListSaysWhichReadingsABackendKeeps).
test("a reading the slice's backend never keeps says so, in the list and in the detail", async () => {
  const [a, b, c] = dgpus.items;
  const withBackends = { items: [
    // Ascend: memory, never compute.
    { ...a, deployment_name: "ascend-slice", sm_util_pct: undefined, sm_util_unsupported: true },
    // A backend that keeps neither reading.
    { ...b, deployment_name: "hygon-slice", memory_used_bytes: undefined, sm_util_pct: undefined, updated_at: undefined, memory_used_unsupported: true, sm_util_unsupported: true },
    // A starting NVIDIA slice: nothing reported yet, both still to come.
    c,
  ] };
  const fetchFixture = globalThis.fetch;
  globalThis.fetch = async (input) => String(input).startsWith("/api/v1/dgpus") ? Response.json(withBackends) : fetchFixture(input);
  try {
    renderPage();
    fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
    const unsupported = gpus.sliceReading.unsupported;
    const ascend = (await screen.findByText("ascend-slice")).closest("tr") as HTMLElement;
    assert.ok(within(ascend).getByText("7.0 GiB"), "the Ascend slice shows the memory it holds");
    assert.equal(within(ascend).getAllByText(unsupported).length, 1, "only its compute reading is unsupported");
    assert.equal(within(ascend).queryByText(gpus.noData), null);
    const hygon = screen.getByText("hygon-slice").closest("tr") as HTMLElement;
    assert.equal(within(hygon).getAllByText(unsupported).length, 2);
    assert.equal(within(hygon).queryByText(gpus.noData), null);
    const starting = screen.getByText("qwen3-dgpu-c").closest("tr") as HTMLElement;
    assert.equal(within(starting).getAllByText(gpus.noData).length, 2, "a reading still to come stays 'No data'");
    assert.equal(within(starting).queryByText(unsupported), null);

    // The explanation is behind a focusable help button, not on the page,
    // and reaching it does not open the row's detail.
    const help = within(ascend).getByRole("button", { name: gpus.sliceReading.unsupportedLabel });
    assert.equal(screen.queryByText(gpus.sliceReading.unsupportedHelp), null);
    help.focus();
    fireEvent.focus(help);
    assert.ok(await screen.findAllByText(gpus.sliceReading.unsupportedHelp));
    fireEvent.click(help);
    assert.equal(screen.queryByRole("dialog"), null);

    // The detail shows the memory it has, says the compute reading is not
    // supported, and charts only what the backend keeps.
    fireEvent.click(screen.getByRole("button", { name: "ascend-slice" }));
    const drawer = await screen.findByRole("dialog");
    assert.ok(await within(drawer).findByRole("heading", { name: gpus.detail.currentUse }));
    assert.ok(within(drawer).getByText("7.0 GiB / 8.0 GiB"));
    assert.ok(within(drawer).getByText(unsupported));
    assert.ok(within(drawer).getByRole("heading", { name: gpus.detail.monitoringData }));
    assert.ok(within(drawer).getByText(i18n.t("metric.sliceVRAM", { limit: "8.0 GiB" })));
    assert.equal(within(drawer).queryByText(gpus.metric.smReferenceAvg), null);
  } finally {
    globalThis.fetch = fetchFixture;
  }
});

test("both locales name the unsupported reading and explain it", () => {
  const zh = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/gpus.json", import.meta.url), "utf8"));
  assert.deepEqual(Object.keys(zh.sliceReading).sort(), Object.keys(gpus.sliceReading).sort());
  assert.equal(zh.sliceReading.unsupported, "不支持");
  assert.notEqual(zh.sliceReading.unsupported, zh.noData);
  assert.notEqual(gpus.sliceReading.unsupported, gpus.noData);
  // The column's help no longer says the figure is HAMi's alone.
  for (const locale of [gpus, zh]) assert.ok(!/^Memory the HAMi|^HAMi/.test(locale.tooltip.actualVRAM));
});

const segmentLabels = {
  slice: (name: string, limit: string) => `${name}:${limit}`, wholeCard: (name: string) => `whole:${name}`, overhead: "overhead", sleeping: "sleeping",
  foreign: "foreign", foreignUnknown: "foreign-unknown", headroom: "headroom", headroomHelp: "headroom-help", withheld: "withheld", free: "free", idle: "idle",
};

test("memory segments follow the books: limits, what slices hold beyond them, residue, free", () => {
  const card = gpusPage.items.find((gpu: { index: number }) => gpu.index === 1);
  const labels = { ...segmentLabels };
  const segments = memorySegments(card, labels);
  assert.deepEqual(segments.map((s) => [s.label, s.bytes / GIB]), [["qwen3-dgpu-a:8.0", 8], ["qwen3-dgpu-b:8.0", 8], ["qwen3-dgpu-c:4.0", 4], ["overhead", 1.5], ["free", 0.5]]);
  const whole = gpusPage.items.find((gpu: { index: number }) => gpu.index === 0);
  assert.deepEqual(memorySegments(whole, labels).map((s) => [s.label, s.bytes / GIB]), [["whole:qwen3-fullcard", 45]]);
  // A sleeper keeps its residue, drawn as such and not as its slice.
  const asleep = { ...card, allocated_bytes: 8.5 * GIB, sleeping_bytes: 1 * GIB, available_bytes: 12.5 * GIB, occupants: [card.occupants[0], { ...card.occupants[1], sleep_level: 1 }] };
  assert.deepEqual(memorySegments(asleep, labels).map((s) => [s.label, s.bytes / GIB]), [["qwen3-dgpu-a:8.0", 8], ["overhead", 0.5], ["sleeping", 1], ["free", 12.5]]);
});

// The card as GPU-7824d54f reported it: nothing of ours on it, a vLLM somebody
// started by hand holding 10 GiB. What the platform withholds there is that
// process, a figure the vendor tool agrees with, and the headroom kept beside
// it, which nobody uses - not a sleeping instance. What is left is not on
// offer while the card takes no work.
test("memory withheld on a card is named by its cause, and free is allocatable only where work is taken", () => {
  const labels = { ...segmentLabels };
  const held = {
    memory_total_bytes: 22 * GIB, allocated_bytes: 0, sleeping_bytes: 0, foreign_bytes: 10 * GIB, available_bytes: 8 * GIB, occupants: [],
    schedulable: false, foreign_processes: [{ pid: 3565129, name: "VLLM::EngineCore", memory_bytes: 10 * GIB }],
    allocation_state: "unusable" as const,
  };
  const rows = (gpu: Parameters<typeof memorySegments>[0]) => memorySegments(gpu, labels).map((s) => [s.label, s.bytes / GIB]);
  assert.deepEqual(rows(held), [["foreign", 10], ["headroom", 4], ["idle", 8]]);
  // Only the headroom explains itself: it is the one figure no tool shows.
  assert.deepEqual(memorySegments(held, labels).map((s) => s.help), [undefined, "headroom-help", undefined]);
  // An operator opened the card to scheduling: the same room is now on offer.
  assert.deepEqual(rows({ ...held, schedulable: true }), [["foreign", 10], ["headroom", 4], ["free", 8]]);
  // A sleeper beside the outside process: each keeps its own figure.
  assert.deepEqual(rows({ ...held, schedulable: true, sleeping_bytes: 1 * GIB, available_bytes: 7 * GIB }), [["sleeping", 1], ["foreign", 10], ["headroom", 4], ["free", 7]]);
  // The deployment policy keeps no headroom: no headroom is drawn or
  // explained, and everything the process leaves is free.
  assert.deepEqual(rows({ ...held, available_bytes: 12 * GIB }), [["foreign", 10], ["idle", 12]]);
  assert.deepEqual(rows({ ...held, schedulable: true, available_bytes: 12 * GIB }), [["foreign", 10], ["free", 12]]);
  // Claims have taken part of the headroom's room: what is left of it.
  assert.deepEqual(rows({ ...held, schedulable: true, allocated_bytes: 11 * GIB, available_bytes: 0, occupants: [{ instance_uuid: "i-1", deployment_id: 1, deployment_name: "qwen", state: "running" }] }),
    [["whole:qwen", 11], ["foreign", 10], ["headroom", 1]]);
  // Holders whose memory is not known: the card offers nothing, and no part
  // of it is called headroom.
  assert.deepEqual(rows({ ...held, foreign_bytes: 0, available_bytes: 0, foreign_processes: [{ pid: 7, name: "python3" }] }), [["foreign-unknown", 22]]);
  // Nothing outside the platform and nobody asleep (a node nobody can reach
  // offers nothing): withheld, with no cause invented for it.
  assert.deepEqual(rows({ ...held, foreign_bytes: 0, foreign_processes: undefined, available_bytes: 0 }), [["withheld", 22]]);
  // The same card once its node is gone: the outside process no longer
  // explains why none of it is on offer.
  assert.deepEqual(rows({ ...held, allocation_state: "unknown", available_bytes: 0 }), [["withheld", 22]]);
  // A faulty card with nothing on it is idle, not allocatable.
  assert.deepEqual(rows({ ...held, foreign_bytes: 0, foreign_processes: [], available_bytes: 22 * GIB }), [["idle", 22]]);
});

// The legend is what tells the two withheld figures apart on the page, and
// the headroom's entry carries the explanation; with no headroom kept there
// is neither entry nor explanation.
test("the legend lists outside processes and their headroom apart, the headroom with its explanation", async () => {
  const held = {
    ...gpusPage.items[0], memory_total_bytes: 22 * GIB, allocated_bytes: 0, sleeping_bytes: 0, foreign_bytes: 10 * GIB, available_bytes: 8 * GIB, occupants: [],
    schedulable: false, foreign_processes: [{ pid: 3565129, name: "VLLM::EngineCore", memory_bytes: 10 * GIB }], allocation_state: "unusable",
    load: { memory_used_bytes: 10 * GIB },
  };
  const view = render(<I18nextProvider i18n={i18n}><GPUMemoryComposition gpu={held} /></I18nextProvider>);
  const legend = view.container.querySelector(".gpu-memory-composition-legend") as HTMLElement;
  assert.deepEqual([...legend.querySelectorAll("li")].map((li) => li.textContent), [
    `${gpus.composition.foreign}10.0 GiB`, `${gpus.composition.headroom}4.0 GiB`, `${gpus.composition.idle}8.0 GiB`, `${gpus.composition.usedLegend}10.0 GiB`,
  ]);
  // The headroom is hatched, the process beside it solid: the swatch's colour
  // is set without the background shorthand, which would cancel the hatching
  // the class draws as a background image.
  const swatches = [...legend.querySelectorAll<HTMLElement>(".gpu-memory-swatch")];
  assert.deepEqual(swatches.map((swatch) => swatch.classList.contains("is-overhead")), [false, true, false, false]);
  assert.match(swatches[1].getAttribute("style") ?? "", /background-color:/);
  assert.equal(swatches[1].style.backgroundImage, "");
  const bar = [...view.container.querySelectorAll<HTMLElement>(".gpu-memory-composition-bar > span")];
  assert.deepEqual(bar.map((part) => part.classList.contains("is-overhead")), [false, true, false]);
  assert.match(bar[1].getAttribute("style") ?? "", /background-color:/);
  const tip = within(legend).getByRole("button", { name: i18n.t("composition.helpLabel", { name: gpus.composition.headroom }) });
  const user = userEvent.setup({ document: dom.window.document });
  await user.hover(tip);
  assert.ok(await screen.findByText(/Settings › Nodes & deployments › Deployment/));
  view.unmount();

  const none = render(<I18nextProvider i18n={i18n}><GPUMemoryComposition gpu={{ ...held, available_bytes: 12 * GIB }} /></I18nextProvider>);
  const bare = none.container.querySelector(".gpu-memory-composition-legend") as HTMLElement;
  assert.deepEqual([...bare.querySelectorAll("li")].map((li) => li.textContent), [
    `${gpus.composition.foreign}10.0 GiB`, `${gpus.composition.idle}12.0 GiB`, `${gpus.composition.usedLegend}10.0 GiB`,
  ]);
  assert.equal(within(bare).queryByRole("button"), null);
});

test("the SM reference is averaged over the trailing minute", () => {
  const raw = [{ labels: { gpu: "1" }, points: [[0, 100], [10_000, 0], [20_000, 100], [30_000, 0], [70_000, 0]] as Array<[number, number]> }];
  const [smoothed] = movingAverage(raw, 60_000);
  assert.deepEqual(smoothed.points.map((p) => Math.round(p[1])), [100, 50, 67, 50, 33]);
});

test("a mixed node is summarised by model, not after its first card", () => {
  assert.equal(gpuModelSummary({ gpu_count: 2, gpus: [{ name: "Quadro RTX 8000" }, { name: "NVIDIA GeForce RTX 2080 Ti" }] }), "1 × Quadro RTX 8000 · 1 × GeForce RTX 2080 Ti");
  assert.equal(gpuModelSummary({ gpu_count: 8, gpus: Array.from({ length: 8 }, () => ({ name: "NVIDIA H100" })) }), "8 × H100");
  assert.equal(gpuModelSummary({ gpu_count: 2, gpus: [] }), "2 × GPU");
});

// A slice that has reported one sample is drawn on the range chosen, with the
// SM reference on its 0..100 axis and the VRAM on its 0..limit axis.
test("a slice's history charts hold the selected range as their time axis, a lone sample visible", async () => {
  const before = Date.now();
  const stubbed = globalThis.fetch;
  globalThis.fetch = async (input) => {
    const url = String(input);
    if (!url.startsWith("/api/v1/metrics/query")) throw new Error(`Unexpected request: ${url}`);
    return Response.json({ items: [{ labels: { gpu: "1" }, points: [[before - 20_000, 50]] }], total: 1 });
  };
  try {
    const { container } = render(<I18nextProvider i18n={i18n}><DGPUHistory gpu={dgpus.items[0]} /></I18nextProvider>);
    const echarts = await import("echarts/core");
    const options = async () => {
      await waitFor(() => assert.equal(container.querySelectorAll("div[role=img] canvas").length, 2));
      return [...container.querySelectorAll<HTMLElement>("div[role=img]")].map((host) => echarts.getInstanceByDom(host)!.getOption() as { xAxis: Array<{ min: number; max: number }>; yAxis: Array<{ min: number; max: number }>; series: Array<{ showSymbol: boolean }> });
    };
    const quarter = await options();
    for (const option of quarter) {
      assert.equal(option.xAxis[0].max - option.xAxis[0].min, 15 * 60_000);
      assert.ok(option.xAxis[0].max >= before && option.xAxis[0].max <= Date.now());
      assert.equal(option.series[0].showSymbol, true);
    }
    assert.deepEqual(quarter.map((option) => [option.yAxis[0].min, option.yAxis[0].max]), [[0, dgpus.items[0].memory_limit_bytes], [0, 100]]);

    fireEvent.click(screen.getByText("6h"));
    await waitFor(async () => assert.equal((await options())[0].xAxis[0].max - (await options())[0].xAxis[0].min, 6 * 3_600_000));
  } finally {
    globalThis.fetch = stubbed;
  }
});
