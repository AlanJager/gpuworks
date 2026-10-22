import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n, dom } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });

const { i18n, locale } = await createI18n(["models"]);
const text = locale.models;
const common = locale.common;

// The page subscribes to the shared server-sent event stream; the test fires
// "modelfile" events itself, the same trigger a running download produces.
class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
  static emit(type: string) { for (const listener of Events.listeners.get(type) ?? []) listener(new Event(type)); }
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter } = await import("react-router-dom");
const { default: ModelsPage } = await import("../src/pages/ModelsPage.tsx");

const GiB = 1024 ** 3;
let downloadedGiB = 1;
let modelListRequests = 0;
const patches: unknown[] = [];

// Every list request answers with freshly parsed objects, as a real HTTP
// response does, while the download keeps advancing.
function modelList() {
  return JSON.parse(JSON.stringify([{
    id: 9, source: "modelscope", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B", created_at: "2026-10-01T00:00:00Z",
    operations: [{ type: "openai.chat" }], input_modalities: ["text"],
    nodes: [{
      node_uuid: "n1", node_name: "gpu3", state: "downloading", path: "/data/models/qwen",
      progress: downloadedGiB / 4, size_bytes: 4 * GiB, downloaded: downloadedGiB * GiB,
      transferred_bytes: 0, rate_bps: 1024, acquisition: "download",
    }],
  }]));
}

globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
  const url = new URL(String(input), "http://localhost");
  if (url.pathname === "/api/v1/models") {
    // The list is paged and its rows carry no per-node presence; the open
    // dialog reads the model itself.
    const { nodes: _nodes, ...row } = modelList()[0];
    return Response.json({ items: [{ ...row, presence: { ready_nodes: 0, cached_nodes: 0, ready_storages: 0, in_flight: 1, in_flight_percent: 25, failed: 0 } }], page: 1, page_size: 20, total: 1, summary: { registered: 1, ready_files: 0, sources: 1, weight_gib: 0 } });
  }
  if (url.pathname === "/api/v1/models/9" && !init?.method) { modelListRequests += 1; return Response.json(modelList()[0]); }
  if (url.pathname === "/api/v1/nodes") {
    return Response.json(url.searchParams.has("page")
      ? { items: [], page: Number(url.searchParams.get("page")), page_size: Number(url.searchParams.get("page_size")), total: 0, all: 0, labels: [] }
      : { items: [], total: 0 });
  }
  if (url.pathname === "/api/v1/models/9/inference-config" && init?.method === "PATCH") {
    patches.push(JSON.parse(String(init.body)));
    return Response.json(modelList()[0]);
  }
  if (url.pathname === "/api/v1/me") return Response.json({ role: "admin" });
  if (url.pathname === "/api/v1/storages") return Response.json({ items: [], total: 0 });
  throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url.pathname}`);
}) as typeof fetch;

afterEach(() => {
  cleanup();
  downloadedGiB = 1;
  modelListRequests = 0;
  patches.length = 0;
});

async function pickOption(combobox: HTMLElement, label: string) {
  const user = userEvent.setup({ document: dom.window.document });
  fireEvent.mouseDown(combobox);
  await user.click(await screen.findByText(label, { selector: ".ant-select-item-option-content" }));
}

const selectedTags = (combobox: HTMLElement) =>
  [...(combobox.closest(".ant-select") as HTMLElement).querySelectorAll(".ant-select-selection-item-content")].map((el) => el.textContent);

test("a background model refresh during a download keeps unsaved capability and input modality edits", async () => {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/models?detail=9"]}><ModelsPage /></MemoryRouter></I18nextProvider>);

  const dialog = await screen.findByRole("dialog");
  const operations = await within(dialog).findByRole("combobox", { name: text.detail.operations });
  const modalities = within(dialog).getByRole("combobox", { name: text.detail.inputModalities });
  await waitFor(() => assert.deepEqual(selectedTags(operations), [common.operation.chat]));
  assert.deepEqual(selectedTags(modalities), [common.inputModality.text]);

  await pickOption(operations, common.operation.embeddings);
  await pickOption(modalities, common.inputModality.image);
  assert.deepEqual(selectedTags(operations), [common.operation.chat, common.operation.embeddings]);
  assert.deepEqual(selectedTags(modalities), [common.inputModality.text, common.inputModality.image]);

  // Download progress arrives: the page refreshes the list in the background.
  const before = modelListRequests;
  downloadedGiB = 2;
  Events.emit("modelfile");
  await waitFor(() => assert.ok(modelListRequests > before, "the open model was re-requested"));
  await waitFor(() => assert.ok(within(dialog).getAllByText(/2\.0 GiB \/ 4\.0 GiB/).length > 0, "new progress rendered"));

  assert.deepEqual(selectedTags(operations), [common.operation.chat, common.operation.embeddings], "capability edit survived the refresh");
  assert.deepEqual(selectedTags(modalities), [common.inputModality.text, common.inputModality.image], "input modality edit survived the refresh");

  const user = userEvent.setup({ document: dom.window.document });
  await user.click(within(dialog).getByRole("button", { name: text.action.save }));
  await waitFor(() => assert.equal(patches.length, 1));
  assert.deepEqual(patches[0], {
    operations: [{ type: "openai.chat" }, { type: "openai.embeddings" }],
    input_modalities: ["text", "image"],
  });
});
