import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { installFleet } from "./helpers/fakeFleet.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["models"]);
const models = locale.models;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { DownloadModal } = await import("../src/pages/ModelsPageParts.tsx");
const { APIError } = await import("../src/apiError.ts");

const api = apiModule.api;
const original = { storages: api.storages, downloadModelToStorage: api.downloadModelToStorage, nodesPage: api.nodesPage, nodes: api.nodes };
// The dialogs search nodes on the server (NodePicker); the fake answers as GET /nodes?page=… does,
// and refuses a request for the whole fleet.
afterEach(() => { cleanup(); Object.assign(api, original) });

const model = { id: 9, source: "huggingface", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B", weight_gib: 15.3 } as never;
const nodes = [
  { uuid: "n1", hostname: "gpu3", gpu_count: 8, state: "online" },
  { uuid: "n2", hostname: "node-01", gpu_count: 4, state: "online" },
] as never[];

function storage(over: Record<string, unknown>) {
  return { id: 1, name: "default", type: "local", enabled: true, is_default: true, all_nodes: true, path: "/data", targets: [], nodes: { total: 2, ready: 2, problems: [] }, writable: true, model_count: 0, deployment_ids: [], ...over };
}
function stubStorages(rows: unknown[]) { api.storages = (async () => list(rows)) as never; }
function stubDownload() {
  const calls: unknown[][] = [];
  api.downloadModelToStorage = (async (...args: unknown[]) => { calls.push(args); }) as never;
  return calls;
}
function renderModal(onDone = () => {}) {
  return render(<I18nextProvider i18n={i18n}><DownloadModal model={model} onClose={() => {}} onDone={onDone} /></I18nextProvider>);
}
async function pick(name: string, option: string | RegExp) {
  fireEvent.mouseDown(await screen.findByRole("combobox", { name }));
  fireEvent.click(await screen.findByText(option));
}

beforeEach(() => { installFleet(api as never, nodes.map((n) => ({ ...(n as object), system_labels: [{key:"gpuworks.ai/storage",value:"3"}] })) as never); });

test("with a single writable local storage only the nodes are asked, and several nodes download one file each", async () => {
  stubStorages([storage({})]);
  const calls = stubDownload();
  let done = 0;
  renderModal(() => { done += 1; });
  await screen.findByRole("combobox", { name: models.download.nodes });
  assert.equal(screen.queryByRole("combobox", { name: models.download.storage }), null, "nothing to choose about the storage");
  const ok = screen.getByRole("button", { name: models.download.action }) as HTMLButtonElement;
  assert.equal(ok.disabled, true, "no node picked yet");

  await pick(models.download.nodes, /gpu3/);
  await pick(models.download.nodes, /node-01/);
  fireEvent.click(screen.getByRole("button", { name: models.download.action }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { storage_id: 1, node_uuids: ["n1", "n2"] }]);
  await waitFor(() => assert.equal(done, 1));
});

test("the storage list only offers writable enabled storages and preselects the default", async () => {
  stubStorages([
    storage({ id: 1, name: "default", is_default: true }),
    storage({ id: 2, name: "nvme", is_default: false, enabled: false }),
    storage({ id: 3, name: "archive", type: "nfs", is_default: false, all_nodes: true }),
    storage({ id: 4, name: "ro-nfs", type: "nfs", is_default: false, writable: false }),
  ]);
  renderModal();
  const combo = await screen.findByRole("combobox", { name: models.download.storage });
  assert.ok(await screen.findByText(/default · Local storage/), "the default storage is preselected");
  fireEvent.mouseDown(combo);
  // The option list renders after the dropdown opens; wait for all four entries.
  await waitFor(() => assert.equal(document.querySelectorAll(".ant-select-item-option").length, 4));
  const options = Array.from(document.querySelectorAll(".ant-select-item-option"));
  const byText = (text: string) => options.find((o) => o.textContent?.startsWith(`${text} · `)) as HTMLElement;
  assert.ok(byText("archive"));
  assert.ok(!byText("archive").className.includes("disabled"));
  for (const readOnly of ["nvme", "ro-nfs"]) {
    const option = byText(readOnly);
    assert.ok(option.className.includes("ant-select-item-option-disabled"), `${readOnly} is greyed out`);
    assert.ok(option.textContent?.includes(models.download.storageReadOnly));
  }
});

test("a shared storage requires one chosen writer and sends that exact node", async () => {
  stubStorages([storage({ id: 1 }), storage({ id: 3, name: "archive", type: "nfs", is_default: false })]);
  const calls = stubDownload();
  renderModal();
  await pick(models.download.storage, /archive/);
  assert.ok(await screen.findByText(models.download.sharedHint));
  assert.equal((screen.getByRole("button", { name: models.download.action }) as HTMLButtonElement).disabled, true);
  await pick(models.download.writer, /node-01/);
  fireEvent.click(screen.getByRole("button", { name: models.download.action }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { storage_id: 3, node_uuids: ["n2"] }]);
});

test("the storage can be chosen from the keyboard", async () => {
  stubStorages([storage({ id: 1 }), storage({ id: 3, name: "archive", type: "nfs", is_default: false })]);
  const calls = stubDownload();
  renderModal();
  const combo = await screen.findByRole("combobox", { name: models.download.storage });
  combo.focus();
  fireEvent.keyDown(combo, { key: "ArrowDown", code: "ArrowDown", keyCode: 40 });
  fireEvent.keyDown(combo, { key: "ArrowDown", code: "ArrowDown", keyCode: 40 });
  fireEvent.keyDown(combo, { key: "Enter", code: "Enter", keyCode: 13 });
  assert.ok(await screen.findByText(models.download.sharedHint), "Enter on the highlighted option picked the NFS storage");
  const writer = await screen.findByRole("combobox", { name: models.download.writer });
  writer.focus();
  fireEvent.keyDown(writer, { key: "ArrowDown", code: "ArrowDown", keyCode: 40 });
  fireEvent.keyDown(writer, { key: "Enter", code: "Enter", keyCode: 13 });
  const ok = screen.getByRole("button", { name: models.download.action });
  ok.focus();
  fireEvent.click(ok);
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { storage_id: 3, node_uuids: ["n1"] }]);
});

test("loading, load failure with retry, and the no-writable-storage case", async () => {
  let release: (value: unknown) => void = () => {};
  api.storages = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  const view = renderModal();
  assert.ok(document.querySelector(".ant-spin"), "a spinner while the storages load");
  assert.equal((screen.getByRole("button", { name: models.download.action }) as HTMLButtonElement).disabled, true);
  release(list([storage({ enabled: false })]));
  assert.ok(await screen.findByText(models.download.noWritableStorage), "only a stopped storage: nothing can be written");
  view.unmount();

  let attempts = 0;
  api.storages = (async () => { attempts += 1; if (attempts === 1) throw new Error("boom"); return list([storage({})]); }) as never;
  renderModal();
  const alert = await screen.findByText(/boom/);
  assert.ok(alert.textContent?.includes("Failed to load storages"));
  const retry = within(alert.closest(".ant-alert") as HTMLElement).getByRole("button", { name: models.action.retry });
  await userEvent.click(retry);
  assert.ok(await screen.findByRole("combobox", { name: models.download.nodes }));
  assert.equal(attempts, 2);
});

test("a failed download keeps the dialog open and reports the error", async () => {
  stubStorages([storage({})]);
  api.downloadModelToStorage = (async () => { throw new Error("STORAGE_DISABLED"); }) as never;
  let done = 0;
  renderModal(() => { done += 1; });
  await pick(models.download.nodes, /gpu3/);
  fireEvent.click(screen.getByRole("button", { name: models.download.action }));
  assert.ok(await screen.findByText(/Download failed: STORAGE_DISABLED/));
  assert.equal(done, 0);
});

test("where the model already is cannot be chosen again, and a no-op answer says so instead of 'started'", async () => {
  stubStorages([storage({}), storage({ id: 2, name: "team-nfs", type: "nfs", is_default: false, path: "/mnt/gpuworks/team-nfs" }), storage({ id: 3, name: "scratch-nfs", type: "nfs", is_default: false, path: "/mnt/gpuworks/scratch" })]);
  const held = {
    ...(model as object),
    storages: [{ storage_id: 2, storage_name: "team-nfs", storage_type: "nfs", path: "Qwen/Qwen3-8B", state: "present", acquisition: "imported", size_bytes: 1, downloaded: 1, progress: 1, rate_bps: 0, ready_nodes: 2, cached_nodes: 0, writable: true }],
    nodes: [{ node_uuid: "n1", state: "present", path: "/data/x", storage_id: 1, progress: 1, size_bytes: 1, downloaded: 1, transferred_bytes: 0, rate_bps: 0, acquisition: "download" }],
  } as never;
  const calls: unknown[][] = [];
  api.downloadModelToStorage = (async (...args: unknown[]) => { calls.push(args); return { status: "present" }; }) as never;
  render(<I18nextProvider i18n={i18n}><DownloadModal model={held} onClose={() => {}} onDone={() => {}} /></I18nextProvider>);

  const storageBox = await screen.findByRole("combobox", { name: models.download.storage });
  fireEvent.mouseDown(storageBox);
  const teamOption = (await screen.findByText(`team-nfs · NFS ${models.download.storageHasIt}`)).closest(".ant-select-item") as HTMLElement;
  assert.ok(teamOption.className.includes("ant-select-item-option-disabled"), "a shared storage that has it is not offered");

  // The local storage (preselected, the default): the node that has it is
  // shown but cannot be picked.
  fireEvent.keyDown(storageBox, { key: "Escape" });
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: models.download.nodes }));
  const gpu3 = (await screen.findByText(`gpu3 · 8 GPU ${models.download.storageHasIt}`)).closest(".ant-select-item") as HTMLElement;
  assert.ok(gpu3.className.includes("ant-select-item-option-disabled"));

  // A storage without it downloads; if the server finds it there after all, the
  // user is told it is already present.
  await pick(models.download.storage, /^scratch-nfs/);
  await pick(models.download.writer, /node-01/);
  fireEvent.click(screen.getByRole("button", { name: models.download.action }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { storage_id: 3, node_uuids: ["n2"] }]);
  assert.ok(await screen.findByText(models.download.alreadyPresent.replace("{{storage}}", "scratch-nfs")));
});

test("a failed download shows the server's error in the person's language, not its English fallback", async () => {
  stubStorages([storage({})]);
  api.downloadModelToStorage = (async () => { throw new APIError("STORAGE_EXPORT_ALREADY_REGISTERED", undefined, "english fallback from the server", 409); }) as never;
  renderModal();
  await screen.findByRole("combobox", { name: models.download.nodes });
  await pick(models.download.nodes, /gpu3/);
  fireEvent.click(screen.getByRole("button", { name: models.download.action }));
  const translated = locale.common.apiError.STORAGE_EXPORT_ALREADY_REGISTERED as string;
  assert.ok(await screen.findByText(models.download.failed.replace("{{detail}}", translated)));
  assert.equal(screen.queryByText(/english fallback from the server/), null);
});
