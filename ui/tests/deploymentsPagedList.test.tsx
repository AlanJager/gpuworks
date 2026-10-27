import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "KeyboardEvent", "Document"] as const) {
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
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const deploymentsLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8"));
const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: deploymentsLocale, common: commonLocale } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: DeploymentsPage } = await import("../src/pages/DeploymentsPage.tsx");
import type { DeploymentListItem, DeploymentPage, ListQuery } from "../src/api";
import { list } from "./helpers/listPage.ts";

// The fleet the fake server pages through: 45 deployments, every third one
// failed, the rest running, newest first. The failed ones have no replica
// running and their health check timed out; the rest answer it.
const fleet: DeploymentListItem[] = Array.from({ length: 45 }, (_, i) => {
  const id = 45 - i;
  return {
    id, name: `svc-${id}`, model_id: 1, model_name: id % 5 === 0 ? "Qwen/Qwen3-8B" : "org/other", served_name: `api-${id}`,
    engine: "vllm", node_uuid: "node-1", node_name: "gpu3", desired_state: "active", runtime_state: id % 3 === 0 ? "failed" : "running",
    sleepable: false, replicas: 1, template_deviated: false, created_at: "2026-10-03T00:00:00Z", operations: [],
    ready_replicas: id % 3 === 0 ? 0 : 1,
    health: id % 3 === 0
      ? { status: "timeout", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }
      : { status: "ok", threshold_sec: 60, last: { at_ms: Date.now(), status: "ok", latency_ms: 90 }, recent_mean_ms: 90, recent_ok: 1, recent: [] },
  };
});

// fakeServer answers GET /deployments?page=… the way the server does: search
// over the name, served name and model name, state filter, then the page.
function fakeServer(rows: DeploymentListItem[]) {
  const queries: ListQuery[] = [];
  const serve = async (query: ListQuery): Promise<DeploymentPage> => {
    queries.push(query);
    const q = query.q?.trim().toLowerCase() ?? "";
    const matched = rows.filter((row) =>
      (!q || [row.name, row.served_name, row.model_name].some((name) => name.toLowerCase().includes(q))) &&
      (!query.state?.length || query.state.includes(row.runtime_state)));
    const start = (query.page - 1) * query.page_size;
    return { items: matched.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: matched.length };
  };
  return { queries, serve };
}

const originals = { deploymentsPage: api.deploymentsPage, deployments: api.deployments, nodes: api.nodes, routingPolicies: api.routingPolicies };
afterEach(() => { cleanup(); Object.assign(api, originals); });
after(() => { closeChannels(); dom.window.close(); });

function renderPage(serve: (query: ListQuery) => Promise<DeploymentPage>) {
  api.deploymentsPage = serve;
  api.deployments = async () => { throw new Error("the list page fetched every deployment"); };
  api.nodes = async () => { throw new Error("the list page fetched every node without an open detail"); };
  api.routingPolicies = async () => list([]);
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><DeploymentsPage /></MemoryRouter></I18nextProvider>);
}

const rowNames = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[0].textContent);

test("a legacy empty served name stays empty rather than copying the model name", async () => {
  const server = fakeServer([{ ...fleet[0], served_name: "" }]);
  renderPage(server.serve);
  const name = await screen.findByText("svc-45");
  const headers = screen.getAllByRole("columnheader");
  const index = headers.indexOf(screen.getByRole("columnheader", { name: "Served name" }));
  const cell = within(name.closest("tr")!).getAllByRole("cell")[index];
  assert.equal(cell.textContent, "—");
  assert.equal(within(cell).queryByRole("button"), null);
});

test("the list asks the server for one page and shows the total", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  assert.deepEqual(server.queries[0], { page: 1, page_size: 20, q: "", state: [] });
  assert.equal(rowNames().length, 20);
  assert.ok(screen.getByText("45 deployments"), "the header counts every deployment, not the page");
  // State, health and replicas side by side.
  const cells = (name: string) => within(screen.getByText(name).closest("tr")!).getAllByRole("cell").map((cell) => cell.textContent);
  assert.ok(cells("svc-44").includes("Healthy · 90 ms") && cells("svc-44").includes("1/1"), String(cells("svc-44")));
  assert.ok(cells("svc-45").includes("Timed out") && cells("svc-45").includes("0/1"), String(cells("svc-45")));

  fireEvent.click(screen.getByTitle("3"));
  await screen.findByText("svc-5");
  assert.deepEqual(rowNames(), ["svc-5", "svc-4", "svc-3", "svc-2", "svc-1"]);
  assert.equal(server.queries.at(-1)?.page, 3);
});

test("searching asks the server and starts again from the first page", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  fireEvent.click(screen.getByTitle("2"));
  await screen.findByText("svc-25");

  const search = screen.getByRole("searchbox", { name: deploymentsLocale.page.search });
  await userEvent.setup().type(search, "qwen3-8b{Enter}");
  await waitFor(() => assert.equal(server.queries.at(-1)?.q, "qwen3-8b"));
  assert.equal(server.queries.at(-1)?.page, 1);
  await screen.findByText("svc-45");
  assert.deepEqual(rowNames(), ["svc-45", "svc-40", "svc-35", "svc-30", "svc-25", "svc-20", "svc-15", "svc-10", "svc-5"]);
  assert.ok(screen.getByText("9 deployments"));
});

test("the state column's header filter asks the server for those states", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  const header = screen.getByRole("columnheader", { name: /State/ });
  fireEvent.click(header.querySelector(".ant-table-filter-trigger")!);
  const failed = await screen.findByRole("menuitem", { name: /Failed/ });
  fireEvent.click(failed);
  fireEvent.click(screen.getByRole("button", { name: "OK" }));
  await waitFor(() => assert.deepEqual(server.queries.at(-1)?.state, ["failed"]));
  assert.equal(server.queries.at(-1)?.page, 1);
  await waitFor(() => assert.ok(screen.getByText("15 deployments")));
  assert.ok(rowNames().every((name) => Number(name?.replace("svc-", "")) % 3 === 0));
});

test("sorting a column asks the server to order every row, not just the page", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  fireEvent.click(screen.getByRole("columnheader", { name: /Name/ }));
  await waitFor(() => assert.equal(server.queries.at(-1)?.sort, "name"));
  fireEvent.click(screen.getByRole("columnheader", { name: /Name/ }));
  await waitFor(() => assert.equal(server.queries.at(-1)?.sort, "-name"));
  fireEvent.click(screen.getByRole("columnheader", { name: /Name/ }));
  await waitFor(() => assert.equal(server.queries.at(-1)?.sort, undefined));
});

test("a search that matches nothing says so instead of offering to create a deployment", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  await userEvent.setup().type(screen.getByRole("searchbox", { name: deploymentsLocale.page.search }), "no-such-service{Enter}");
  await screen.findByText(deploymentsLocale.page.emptyFiltered);
  assert.equal(screen.queryByRole("button", { name: deploymentsLocale.page.create }), null);
});

test("with no deployments at all the empty state offers to create one", async () => {
  renderPage(fakeServer([]).serve);
  await screen.findByText(deploymentsLocale.page.empty);
  assert.ok(screen.getByRole("button", { name: deploymentsLocale.page.create }));
});

test("a failed page load shows the error and retries on request", async () => {
  let calls = 0;
  const server = fakeServer(fleet);
  renderPage(async (query) => {
    calls += 1;
    if (calls === 1) throw new Error("list unavailable");
    return server.serve(query);
  });
  await screen.findByText("list unavailable");
  fireEvent.click(screen.getByRole("button", { name: commonLocale.actions.retry }));
  await screen.findByText("svc-45");
});


test("replica column labels ready versus desired for PD and ordinary rows, preserving unknown readiness", async () => {
  renderPage(fakeServer([
    { ...fleet[0], id: 101, name: "pd-ready", replicas: 2, ready_replicas: 2 },
    { ...fleet[0], id: 102, name: "pd-partial", replicas: 3, ready_replicas: 1 },
    { ...fleet[0], id: 103, name: "ordinary-pending", replicas: 1, ready_replicas: 0 },
    { ...fleet[0], id: 104, name: "unknown-readiness", replicas: 2, ready_replicas: undefined },
  ]).serve);
  await screen.findByText("pd-ready");
  assert.ok(screen.getByRole("columnheader", { name: /Ready \/ desired replicas/ }));
  for (const [name, count] of [["pd-ready", "2/2"], ["pd-partial", "1/3"], ["ordinary-pending", "0/1"], ["unknown-readiness", "—/2"]]) {
    assert.ok(within(screen.getByText(name).closest("tr")!).getByRole("cell", { name: count }));
  }
});
