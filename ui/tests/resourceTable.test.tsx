import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, beforeEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

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

const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { common: commonLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ResourceTable, compareSortValues } = await import("../src/components/resourceTable/ResourceTable.tsx");
const { parseColumnPrefs, arrangeColumns, defaultColumnPrefs, storageKey } = await import("../src/components/resourceTable/columnPrefs.ts");
import type { ResourceColumn } from "../src/components/resourceTable/ResourceTable";

beforeEach(() => localStorage.clear());
afterEach(() => cleanup());
after(() => { closeChannels(); dom.window.close(); });

type Row = { id: number; name: string; gpus: number; state: string };
const rows: Row[] = Array.from({ length: 45 }, (_, i) => ({ id: i + 1, name: `node-${String(i + 1).padStart(2, "0")}`, gpus: (i * 7) % 9, state: i % 3 === 0 ? "offline" : "online" }));

function columns(): ResourceColumn<Row>[] {
  return [
    { key: "name", role: "name", title: "Name", width: 200, sortValue: (row) => row.name, render: (_, row) => row.name },
    { key: "gpus", title: "GPUs", width: 100, sortValue: (row) => row.gpus, render: (_, row) => row.gpus },
    { key: "state", title: "State", width: 120, filters: [{ value: "online", text: "Online" }, { value: "offline", text: "Offline" }], onFilter: (value, row) => row.state === value, render: (_, row) => row.state },
    { key: "zone", title: "Zone", width: 120, defaultHidden: true, render: () => "z1" },
    { key: "actions", role: "actions", width: 90, render: (_, row) => <button type="button">Open {row.name}</button> },
  ];
}

function renderTable(props: Partial<React.ComponentProps<typeof ResourceTable<Row>>> = {}) {
  return render(
    <I18nextProvider i18n={i18n}>
      <ResourceTable<Row> tableId="test" rowKey="id" columns={columns()} data={rows} {...props} />
    </I18nextProvider>,
  );
}

const headerTexts = () => screen.getAllByRole("columnheader").map((cell) => cell.textContent);
const firstCells = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[0].textContent);

test("the name column leads frozen left and the actions column trails frozen right under its own header", () => {
  renderTable();
  const headers = screen.getAllByRole("columnheader");
  assert.deepEqual(headerTexts(), ["Name", "GPUs", "State", "Actions"]);
  assert.ok(headers[0].className.includes("ant-table-cell-fix-left"));
  assert.ok(headers.at(-1)!.className.includes("ant-table-cell-fix-right"));
  const table = document.querySelector(".ant-table-content table") as HTMLTableElement;
  assert.equal(table.style.tableLayout, "fixed");
  assert.equal(table.style.width, "510px", "the scroll width is the sum of the shown columns");
});

test("the page size can be changed and is remembered", async () => {
  renderTable();
  assert.equal(firstCells().length, 20);
  fireEvent.mouseDown(document.querySelector(".ant-pagination-options .ant-select-selector")!);
  const option = await waitFor(() => {
    const found = Array.from(document.querySelectorAll(".ant-select-item-option")).find((item) => item.textContent === "50 / page");
    assert.ok(found);
    return found;
  });
  fireEvent.click(option);
  await waitFor(() => assert.equal(firstCells().length, 45));
  assert.equal(JSON.parse(localStorage.getItem(storageKey("test"))!).pageSize, 50);
  cleanup();
  renderTable();
  assert.equal(firstCells().length, 45, "the size survives a reload");
});

test("a client list sorts by the column's value and filters from the header", async () => {
  renderTable({ data: rows.slice(0, 6) });
  fireEvent.click(screen.getByRole("columnheader", { name: /GPUs/ }));
  const gpus = () => screen.getAllByRole("row").slice(1).map((row) => Number(within(row).getAllByRole("cell")[1].textContent));
  assert.deepEqual(gpus(), [...gpus()].sort((a, b) => a - b));
  fireEvent.click(screen.getByRole("columnheader", { name: /GPUs/ }));
  assert.deepEqual(gpus(), [...gpus()].sort((a, b) => b - a));

  fireEvent.click(screen.getByRole("columnheader", { name: /State/ }).querySelector(".ant-table-filter-trigger")!);
  fireEvent.click(await screen.findByRole("menuitem", { name: /Offline/ }));
  fireEvent.click(screen.getByRole("button", { name: "OK" }));
  await waitFor(() => assert.deepEqual(firstCells().sort(), ["node-01", "node-04"]));
});

test("columns can be hidden, shown and restored from the settings drawer", async () => {
  const user = userEvent.setup();
  renderTable();
  await user.click(screen.getByRole("button", { name: "Customize columns" }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("button", { name: "Hide GPUs" }));
  await waitFor(() => assert.deepEqual(headerTexts(), ["Name", "State", "Actions"]));
  await user.click(within(drawer).getByRole("checkbox", { name: "Zone" }));
  await waitFor(() => assert.deepEqual(headerTexts(), ["Name", "State", "Zone", "Actions"]));
  assert.equal((within(drawer).getByRole("checkbox", { name: "Name" }) as HTMLInputElement).disabled, true, "the name column never hides");
  const saved = JSON.parse(localStorage.getItem(storageKey("test"))!);
  assert.deepEqual(saved.hidden, ["gpus"]);

  await user.click(within(drawer).getByRole("button", { name: "Restore defaults" }));
  await waitFor(() => assert.deepEqual(headerTexts(), ["Name", "GPUs", "State", "Actions"]));
});

test("a header resize handle changes and remembers the column width; the keyboard can do it too", async () => {
  renderTable();
  const handle = screen.getByRole("separator", { name: "Resize GPUs" });
  handle.focus();
  fireEvent.keyDown(handle, { key: "ArrowRight" });
  await waitFor(() => assert.equal(JSON.parse(localStorage.getItem(storageKey("test"))!).widths.gpus, 116));
  for (let i = 0; i < 4; i++) fireEvent.keyDown(screen.getByRole("separator", { name: "Resize GPUs" }), { key: "ArrowLeft" });
  await waitFor(() => assert.equal(JSON.parse(localStorage.getItem(storageKey("test"))!).widths.gpus, 64), "a column never narrows past its minimum");
  assert.equal(screen.queryByRole("separator", { name: /Actions/ }), null, "the actions column keeps its width");
});

test("a server-sorted list hands the sort to its owner instead of sorting the page", async () => {
  const sorts: unknown[] = [];
  const cols = columns().map((column) => (column.key === "name" ? { ...column, sortValue: undefined, serverSort: true } : column));
  renderTable({ columns: cols, data: rows.slice(0, 3).reverse(), sort: null, onSortChange: (sort) => sorts.push(sort), pagination: { current: 1, pageSize: 20, total: 3 } });
  fireEvent.click(screen.getByRole("columnheader", { name: /Name/ }));
  assert.deepEqual(sorts, [{ key: "name", order: "asc" }]);
  assert.deepEqual(firstCells(), ["node-03", "node-02", "node-01"], "the rows stay as the server sent them");
});

test("loading shows no false empty state; an error offers a keyboard-operable retry", async () => {
  const { container } = renderTable({ data: [], loading: true, emptyDescription: "Nothing here" });
  assert.ok(container.querySelector(".ant-spin-spinning"));
  assert.equal(screen.queryByText("Nothing here"), null);
  cleanup();
  renderTable({ data: [], emptyDescription: "Nothing here" });
  assert.ok(screen.getByText("Nothing here"));
  cleanup();
  let retries = 0;
  renderTable({ error: new Error("network down"), onRefresh: () => retries++ });
  assert.ok(screen.getByRole("alert").textContent?.includes("network down"));
  screen.getByRole("button", { name: "Retry" }).focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(retries, 1);
});

test("saved preferences keep the viewer's choices across column changes and fall back when unreadable", () => {
  const cols = columns();
  assert.deepEqual(parseColumnPrefs("{not json", cols), defaultColumnPrefs(cols));
  assert.deepEqual(parseColumnPrefs(JSON.stringify({ order: "x" }), cols), defaultColumnPrefs(cols));
  // A saved layout from before the zone column existed, naming a column that is gone.
  const saved = JSON.stringify({ order: ["state", "gpus", "removed"], hidden: ["gpus", "removed"], widths: { gpus: 10, state: 300.4, removed: 50 }, pageSize: 37 });
  const prefs = parseColumnPrefs(saved, cols);
  // Zone follows state by default, wherever the viewer moved state.
  assert.deepEqual(prefs.order, ["state", "zone", "gpus"], "a new column follows its default predecessor");
  assert.deepEqual(prefs.hidden, ["zone", "gpus"], "a new column takes its default visibility");
  assert.deepEqual(prefs.widths, { gpus: 64, state: 300 }, "widths are clamped and rounded; unknown ones dropped");
  assert.equal(prefs.pageSize, undefined, "an unoffered page size is ignored");
  assert.deepEqual(arrangeColumns(cols, prefs).map((column) => column.key), ["name", "state", "actions"]);
});

test("sort values put missing values first ascending and compare names naturally", () => {
  // Wrapped: Array.prototype.sort moves bare undefined to the end without asking the comparator.
  const sorted = (values: Array<string | number | null | undefined>) => values.map((v) => ({ v })).sort((a, b) => compareSortValues(a.v, b.v)).map(({ v }) => v);
  assert.deepEqual(sorted(["node-10", "node-9", undefined, "Node-1"]), [undefined, "Node-1", "node-9", "node-10"]);
  assert.deepEqual(sorted([3, null, 1, ""]), [null, "", 1, 3]);
});
