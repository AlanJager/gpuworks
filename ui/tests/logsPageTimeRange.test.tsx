import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { LogEntry } from "../src/api";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "zh", defaultNS: "logs", interpolation: { escapeValue: false },
  resources: { zh: { logs: (await import("../src/i18n/locales/zh/logs.json")).default } },
});
const { cleanup, render, screen, fireEvent, waitFor, within } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { default: LogsPage } = await import("../src/pages/LogsPage.tsx");

let entries: LogEntry[] = [];
let failSearch = false;
const requests: URL[] = [];
const dayMS = 86_400_000;
const entry = (id: number, time_ms: number, line: string): LogEntry => ({ id, time_ms, line, instance: "inst-a", node: "node-a", source: "instance", level: "error" });

// Apply actual time/source/keyword/cursor semantics instead of returning a
// fixed page. In particular, pages without with_total omit Total entirely.
globalThis.fetch = (async (input: RequestInfo | URL) => {
  const url = new URL(String(input), "http://localhost");
  if (url.pathname === "/api/v1/instances") return Response.json({ items: [], total: 0 });
  if (url.pathname === "/api/v1/nodes") return Response.json({ items: [], page: 1, page_size: 50, total: 0, all: 0, labels: [] });
  assert.ok(["/api/v1/logs/search", "/api/v1/logs/histogram"].includes(url.pathname), `unexpected request: ${url}`);
  requests.push(url);
  const p = url.searchParams;
  const q = p.get("q") || "";
  assert.ok(q === "" || q === "Traceback", `unsupported fixture query: ${q}`);
  const source = p.get("source") ?? "instance";
  let matches = entries.filter((e) => (!q || e.line.includes(q)) && (!source || e.source === source)
    && (!p.has("from") || e.time_ms >= Number(p.get("from"))) && (!p.has("to") || e.time_ms <= Number(p.get("to"))));
  if (url.pathname.endsWith("histogram")) {
    const bucketMS = Number(p.get("bucket"));
    const counts = new Map<number, number>();
    for (const e of matches) {
      const start = Math.floor(e.time_ms / bucketMS) * bucketMS;
      counts.set(start, (counts.get(start) ?? 0) + 1);
    }
    const buckets = [...counts].map(([start_ms, count]) => ({ start_ms, counts: { error: count } }));
    return Response.json({ items: buckets, total: buckets.length });
  }
  if (failSearch) return Response.json({ error: "search unavailable" }, { status: 500 });
  if (p.has("cursor_ts")) {
    const cursorDay = Math.floor(Number(p.get("cursor_ts")) / dayMS) * dayMS;
    matches = matches.filter((e) => e.time_ms < cursorDay || (e.time_ms < cursorDay + dayMS && e.id < Number(p.get("cursor_id"))));
  }
  matches.sort((a, b) => Math.floor(b.time_ms / dayMS) - Math.floor(a.time_ms / dayMS) || b.id - a.id);
  const limit = Number(p.get("limit") || 200);
  return Response.json({ items: matches.slice(0, limit), ...(p.has("with_total") && matches.length ? { total: matches.length } : {}), ...(matches.length > limit ? { truncated: true } : {}) });
}) as typeof fetch;

beforeEach(() => { entries = []; requests.length = 0; failSearch = false; });
afterEach(() => cleanup());

function mount(path = "/logs") {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[path]}><LogsPage /></MemoryRouter></I18nextProvider>);
}

async function pickRange(label: string) {
  fireEvent.mouseDown(within(screen.getByTestId("range-select")).getByRole("combobox"));
  fireEvent.click(await screen.findByText(label, { selector: ".ant-select-item-option-content" }));
}

async function expectRange(from: string | null, to: string | null) {
  await waitFor(() => {
    for (const kind of ["search", "histogram"]) {
      const request = requests.filter((r) => r.pathname.endsWith(kind)).at(-1);
      assert.ok(request, `missing ${kind}`);
      assert.equal(request.searchParams.get("from"), from, `${kind} from`);
      assert.equal(request.searchParams.get("to"), to, `${kind} to`);
    }
  });
}

test("the console defaults to 24h; seven days and all share bounds with totals and the histogram", async () => {
  const now = Date.now();
  entries = [entry(1, now - 10 * dayMS, "Traceback old"), entry(1, now - 3 * dayMS, "Traceback week"), entry(1, now - 3_600_000, "Traceback recent")];
  mount();
  await screen.findByText("命中 1 条");
  assert.ok(within(screen.getByTestId("range-select")).getByText("最近 24 小时"));
  const defaultFrom = requests.find((r) => r.pathname.endsWith("search"))!.searchParams.get("from");
  assert.ok(Number(defaultFrom) >= now - dayMS && Number(defaultFrom) <= Date.now() - dayMS);
  await expectRange(defaultFrom, null);
  fireEvent.click(screen.getByText("Traceback"));
  await waitFor(() => assert.equal(requests.filter((r) => r.pathname.endsWith("histogram")).at(-1)!.searchParams.get("q"), "Traceback"));

  // The next preset after 24h is 7d; exercise the Select's keyboard path.
  const combo = within(screen.getByTestId("range-select")).getByRole("combobox");
  fireEvent.mouseDown(combo);
  await waitFor(() => assert.equal(document.querySelector(".ant-select-item-option-active .ant-select-item-option-content")?.textContent, "最近 24 小时"));
  fireEvent.keyDown(combo, { key: "ArrowDown", keyCode: 40 });
  fireEvent.keyDown(combo, { key: "Enter", keyCode: 13 });
  await screen.findByText("命中 2 条");
  const weekFrom = requests.filter((r) => r.pathname.endsWith("search")).at(-1)!.searchParams.get("from");
  assert.ok(Number(weekFrom) >= now - 7 * dayMS && Number(weekFrom) <= Date.now() - 7 * dayMS);
  await expectRange(weekFrom, null);
  assert.equal(screen.getAllByTestId("log-row").length, 2);

  await pickRange("全部");
  await screen.findByText("命中 3 条");
  await expectRange(null, null);
  assert.equal(screen.getAllByTestId("log-row").length, 3);
  assert.ok(screen.getAllByTestId("log-row").some((row) => row.textContent?.includes("Traceback old")));
  await waitFor(() => assert.equal(within(screen.getByTestId("logs-histogram")).getAllByRole("img").length, 3));

  await pickRange("最近 24 小时");
  await screen.findByText("命中 1 条");
  assert.equal(screen.getAllByTestId("log-row").length, 1);
});

test("pagination preserves a custom upper bound and the total for the whole range", async () => {
  const from = 1_700_000_000_000;
  const to = from + 10_000;
  // The excluded row arrived before the first 500 hits, but its timestamp
  // is newer than the chosen window. A rowid cursor alone cannot exclude it.
  entries = [entry(1, from + 100, "oldest in window"), entry(2, to + 1000, "outside window")];
  for (let id = 3; id <= 502; id++) entries.push(entry(id, from + 500, `in window ${id}`));
  mount(`/logs?from=${from}&to=${to}`);
  await screen.findByText("命中 501 条");
  await expectRange(String(from), String(to));
  fireEvent.click(screen.getByRole("button", { name: /加载更多/ }));
  await screen.findByText("oldest in window");
  assert.equal(screen.queryByText("outside window"), null);
  assert.ok(screen.getByText("命中 501 条"));
  assert.equal(screen.getAllByTestId("log-row").length, 501);
  const appended = requests.filter((r) => r.pathname.endsWith("search")).at(-1)!.searchParams;
  assert.equal(appended.get("to"), String(to));
  assert.equal(appended.get("from"), String(from));
  assert.equal(appended.has("with_total"), false);
});

test("all-time search displays empty and error responses", async () => {
  mount();
  await screen.findByText("暂无日志");
  await pickRange("全部");
  await expectRange(null, null);
  assert.ok(screen.getByText("暂无日志"));
  failSearch = true;
  fireEvent.click(screen.getByRole("button", { name: /刷\s*新/ }));
  await screen.findByText(/search unavailable/);
});

test("自定义 picks an absolute start and end; the query keeps its window until both are confirmed", async () => {
  const day = (iso: string) => new Date(iso).getTime();
  entries = [entry(1, day("2026-10-18T10:30:00"), "Traceback inside"), entry(2, day("2026-10-18T12:30:00"), "Traceback after"), entry(3, Date.now() - 60_000, "Traceback now")];
  mount();
  await screen.findByText("命中 1 条");
  const before = requests.length;
  assert.equal(document.getElementById("logs-range-start"), null);
  await pickRange("自定义");
  // Choosing 自定义 only reveals the picker: nothing is queried yet.
  await waitFor(() => assert.ok(document.getElementById("logs-range-start")));
  assert.equal(requests.length, before);
  const start = document.getElementById("logs-range-start") as HTMLInputElement;
  const end = document.getElementById("logs-range-end") as HTMLInputElement;
  fireEvent.mouseDown(start);
  fireEvent.focus(start);
  fireEvent.change(start, { target: { value: "2026-10-18 10:00:00" } });
  fireEvent.click(await screen.findByRole("button", { name: "OK" }));
  fireEvent.focus(end);
  fireEvent.change(end, { target: { value: "2026-10-18 11:00:00" } });
  fireEvent.click((await screen.findAllByRole("button", { name: "OK" })).at(-1)!);
  await screen.findByText("Traceback inside");
  await expectRange(String(day("2026-10-18T10:00:00")), String(day("2026-10-18T11:00:00")));
  assert.equal(screen.getAllByTestId("log-row").length, 1);
  assert.ok(within(screen.getByTestId("range-select")).getByText("自定义"));

  // Going back to a preset drops the absolute window and the picker.
  await pickRange("最近 24 小时");
  await screen.findByText("Traceback now");
  assert.equal(document.getElementById("logs-range-start"), null);
});

test("a deep-linked window shows in the picker as the custom range", async () => {
  const from = new Date("2026-10-18T10:00:00").getTime();
  mount(`/logs?from=${from}&to=${from + 3_600_000}`);
  await expectRange(String(from), String(from + 3_600_000));
  assert.ok(within(screen.getByTestId("range-select")).getByText("自定义"));
  const inputs = ["logs-range-start", "logs-range-end"].map((id) => document.getElementById(id) as HTMLInputElement);
  assert.deepEqual(inputs.map((i) => i.value), ["2026-10-18 10:00:00", "2026-10-18 11:00:00"]);
});
