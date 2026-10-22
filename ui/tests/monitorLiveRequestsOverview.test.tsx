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

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: LiveRequestsOverview, stalledFor } = await import("../src/pages/monitor/LiveRequestsOverview.tsx");
import type { MonitorReplica } from "../src/api";
import type { MonitorRequest, MonitorRequests } from "../src/monitorRequestsApi";

const original = api.monitorRequests;
afterEach(() => {
  cleanup();
  api.monitorRequests = original;
});
after(() => {
  closeChannels();
  dom.window.close();
});

function req(id: string, over: Partial<MonitorRequest>): MonitorRequest {
  return {
    id, received_ms: 0, phase: "waiting", age_ms: 1000, instance_uuid: "i-a", api_key_id: 3, api_key_name: "team-a",
    user_id: 1, source: "api", stream: true, body_bytes: 100, images: 0,
    dispatch_ms: 1, first_byte_ms: null, first_think_ms: null, first_answer_ms: null, last_chunk_ms: null, end_ms: null,
    events: 0, think_events: 0, max_gap_ms: null, status: null, outcome: "", finish_reason: "", result: "",
    prompt_tokens: null, cached_tokens: null, completion_tokens: null, reasoning_tokens: null,
    engine_queue_ms: null, engine_prefill_ms: null, engine_decode_ms: null, weight: 1, segments: [],
    ...over,
  };
}

const replicas = [
  { instance_uuid: "i-a", node_name: "gpu-1" },
  { instance_uuid: "i-b", node_name: "gpu-2" },
] as MonitorReplica[];

function live(requests: MonitorRequest[], counts = { waiting: 0, thinking: 0, answering: 0 }): MonitorRequests {
  return { view: "live", as_of: 1, counts, requests, next_cursor: "" };
}

function renderOverview() {
  return render(
    <I18nextProvider i18n={i18n}>
      <LiveRequestsOverview deploymentId={7} replicas={replicas} />
    </I18nextProvider>,
  );
}

test("a request streaming has stalled for as long as it sent nothing; one still waiting has not", () => {
  assert.equal(stalledFor(req("a", { phase: "answering", age_ms: 9000, last_chunk_ms: 4000 })), 5000);
  assert.equal(stalledFor(req("b", { phase: "waiting", age_ms: 9000 })), null);
  assert.equal(stalledFor(req("c", { phase: "thinking", age_ms: 9000, last_chunk_ms: null })), null);
});

test("the phase bar counts every request in flight and the swimlane draws each on its phase's lane", async () => {
  const queries: unknown[] = [];
  api.monitorRequests = async (id, query) => {
    queries.push([id, query]);
    return live(
      [
        req("w1", { phase: "waiting", age_ms: 12_000 }),
        req("w2", { phase: "waiting", age_ms: 3000 }),
        req("t1", { phase: "thinking", age_ms: 5000, last_chunk_ms: 4900, events: 40 }),
        req("a1", { phase: "answering", age_ms: 20_000, last_chunk_ms: 10_000, events: 300, instance_uuid: "i-b" }),
        req("a2", { phase: "answering", age_ms: 8000, last_chunk_ms: 7990, events: 120 }),
      ],
      { waiting: 2, thinking: 1, answering: 2 },
    );
  };
  const { container } = renderOverview();
  await screen.findByText("5");
  assert.deepEqual(queries[0], [7, { view: "live", limit: 500 }]);

  const bar = screen.getByRole("list", { name: "Requests in flight by phase" });
  const parts = Array.from(bar.querySelectorAll<HTMLElement>("[role=listitem]"));
  assert.deepEqual(parts.map((p) => [p.dataset.kind, p.style.flexGrow]), [["waiting", "2"], ["thinking", "1"], ["answering", "2"]]);
  screen.getByText("longest wait for a first token 12 s");
  // Only a1 has sent nothing for over 2 s.
  screen.getByText("1 stalled (nothing new for over 2 s)");

  const dots = Array.from(container.querySelectorAll<SVGGElement>("g[data-request]"));
  assert.deepEqual(dots.map((g) => [g.dataset.request, g.dataset.lane]), [["w1", "waiting"], ["w2", "waiting"], ["t1", "thinking"], ["a1", "answering"], ["a2", "answering"]]);
  assert.deepEqual(dots.filter((g) => g.dataset.stalled).map((g) => g.dataset.request), ["a1"]);
  // Older requests sit further right.
  const cx = (id: string) => Number(container.querySelector(`g[data-request=${id}] circle`)!.getAttribute("cx"));
  assert.ok(cx("w1") > cx("w2"));
  screen.getByRole("img", { name: "5 requests in flight" });
});

test("hovering a dot says whose request it is, where it runs and how long it has been silent", async () => {
  api.monitorRequests = async () =>
    live([req("a1", { phase: "answering", age_ms: 20_000, last_chunk_ms: 10_000, events: 300, instance_uuid: "i-b" })], { waiting: 0, thinking: 0, answering: 1 });
  const { container } = renderOverview();
  await waitFor(() => assert.ok(container.querySelector("g[data-request=a1]")));
  const hit = container.querySelectorAll("g[data-request=a1] circle")[1];
  fireEvent.mouseEnter(hit);
  const tip = screen.getByRole("tooltip");
  assert.match(tip.textContent ?? "", /team-a/);
  assert.match(tip.textContent ?? "", /Answering · 20 s/);
  assert.match(tip.textContent ?? "", /Replica 2 · gpu-2/);
  assert.match(tip.textContent ?? "", /nothing new for 10 s/);
  // The tooltip is drawn outside the card, whose body would clip it, fixed
  // in the viewport; the plot stays a short strip.
  assert.equal(tip.closest(".ant-card") === null, true);
  assert.equal(tip.parentElement === document.body, true);
  assert.equal(container.contains(tip), false);
  assert.equal(tip.style.position === "" || tip.style.position === "fixed", true);
  assert.equal(/^-?\d+(\.\d+)?px$/.test(tip.style.top) && /^-?\d+(\.\d+)?px$/.test(tip.style.left), true);
  assert.equal(tip.style.visibility, "visible");
  const svgH = Number(container.querySelector(".monitor-swimlane-plot > svg")!.getAttribute("height"));
  assert.equal(svgH <= 130, true, "the plot is a short strip");
  fireEvent.mouseLeave(hit);
  assert.equal(screen.queryByRole("tooltip")?.textContent ?? null, null);
});

test("a dot near the bottom right corner of the screen gets its tooltip above it, inside the viewport", async () => {
  api.monitorRequests = async () => live([req("a1", { phase: "answering", age_ms: 20_000, last_chunk_ms: 10_000, events: 300 })], { waiting: 0, thinking: 0, answering: 1 });
  const { container } = renderOverview();
  await waitFor(() => assert.ok(container.querySelector("g[data-request=a1]")));
  const hit = container.querySelectorAll("g[data-request=a1] circle")[1];
  const original = Element.prototype.getBoundingClientRect;
  // A 200x80 box wherever it is asked, the dot at the screen's bottom right.
  Element.prototype.getBoundingClientRect = function (this: Element) {
    const dot = this === hit;
    const box = dot ? { left: 1000, right: 1010, top: 700, bottom: 710, width: 10, height: 10 } : { left: 0, right: 200, top: 0, bottom: 80, width: 200, height: 80 };
    return { ...box, x: box.left, y: box.top, toJSON: () => box } as DOMRect;
  };
  try {
    fireEvent.mouseEnter(hit);
    const tip = screen.getByRole("tooltip");
    const top = parseFloat(tip.style.top);
    const left = parseFloat(tip.style.left);
    assert.equal(top + 80 <= 700, true, `above the dot, got top ${top}`);
    assert.equal(left >= 8 && left + 200 <= window.innerWidth - 8, true, `inside the viewport, got left ${left}`);
  } finally {
    Element.prototype.getBoundingClientRect = original;
  }
});

test("an idle service shows nothing in flight; more in flight than drawn says so", async () => {
  api.monitorRequests = async () => live([]);
  renderOverview();
  await screen.findByRole("img", { name: "0 requests in flight" });
  screen.getByText("0 stalled (nothing new for over 2 s)");
  screen.getByText("longest wait for a first token —");
  cleanup();

  api.monitorRequests = async () => live([req("w1", { age_ms: 900 })], { waiting: 700, thinking: 0, answering: 0 });
  renderOverview();
  await screen.findByText("700 in flight; the 1 most worth a look are drawn (longest waits first)");
});

test("a failed poll leaves the counts unknown rather than zero", async () => {
  api.monitorRequests = async () => {
    throw new Error("boom");
  };
  renderOverview();
  await new Promise((r) => setTimeout(r, 20));
  assert.ok(screen.getAllByText("—").length >= 4);
});
