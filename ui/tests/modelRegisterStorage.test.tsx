import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { installFleet } from "./helpers/fakeFleet.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["models"]);
const models = locale.models;
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const apiModule = await import("../src/api.ts");
const { RegisterPathModal } = await import("../src/pages/ModelsPageParts.tsx");
const { RegisterModal } = await import("../src/pages/models/RegisterModelModal.tsx");

const api = apiModule.api;
const original = { storages: api.storages, registerModelPath: api.registerModelPath, registerLocalModel: api.registerLocalModel, previewLocalModel: api.previewLocalModel, imageVersions: api.imageVersions, nodesPage: api.nodesPage, nodes: api.nodes };
// The dialogs search nodes on the server (NodePicker); the fake answers as GET /nodes?page=… does,
// and refuses a request for the whole fleet.
afterEach(() => { cleanup(); Object.assign(api, original) });

function storage(over: Record<string, unknown>) {
  return { id: 1, name: "default", type: "local", enabled: true, is_default: true, all_nodes: true, path: "/data", targets: [], nodes: { total: 1, ready: 1, problems: [] }, writable: true, model_count: 0, deployment_ids: [], ...over };
}
const oneLocal = [storage({}), storage({ id: 3, name: "archive", type: "nfs", is_default: false }), storage({ id: 2, name: "off", is_default: false, enabled: false })];
const twoLocal = [storage({}), storage({ id: 2, name: "nvme", is_default: false })];
const nodes = [{ uuid: "n1", hostname: "gpu3", gpu_count: 8, state: "online" }] as never[];
const model = { id: 9, source: "modelscope", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B" } as never;

function stubPath() {
  const calls: unknown[][] = [];
  api.registerModelPath = (async (...args: unknown[]) => { calls.push(args); }) as never;
  return calls;
}
async function fillPath(path = "/models/qwen") {
  fireEvent.mouseDown(screen.getAllByRole("combobox").at(-1)!); // the node select has only a placeholder
  fireEvent.click(await screen.findByText(/gpu3/));
  fireEvent.change(screen.getByPlaceholderText("/models/Qwen/Qwen3-8B"), { target: { value: path } });
}
function renderPath() {
  return render(<I18nextProvider i18n={i18n}><RegisterPathModal model={model} onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
}

beforeEach(() => { installFleet(api as never, nodes as never); });

test("with one enabled local storage the path dialog has no storage control and the request stays as before", async () => {
  api.storages = (async () => list(oneLocal)) as never;
  const calls = stubPath();
  renderPath();
  await new Promise((resolve) => setTimeout(resolve, 30));
  assert.equal(screen.queryByRole("combobox", { name: models.register.storage }), null);
  await fillPath();
  fireEvent.click(screen.getByRole("button", { name: models.path.register }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, "n1", "/models/qwen"], "no storage_id argument at all");
});

test("with several local storages the path dialog asks which one, preselects the default and sends the choice", async () => {
  api.storages = (async () => list(twoLocal)) as never;
  const calls = stubPath();
  renderPath();
  const combo = await screen.findByRole("combobox", { name: models.register.storage });
  assert.ok(await screen.findByText("default"), "the default storage is preselected");
  fireEvent.mouseDown(combo);
  fireEvent.click(await screen.findByText("nvme"));
  await fillPath();
  fireEvent.click(screen.getByRole("button", { name: models.path.register }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, "n1", "/models/qwen", 2]);
});

test("the path dialog still works when the storages cannot be loaded", async () => {
  api.storages = (async () => { throw new Error("down"); }) as never;
  const calls = stubPath();
  renderPath();
  await new Promise((resolve) => setTimeout(resolve, 30));
  await fillPath();
  fireEvent.click(screen.getByRole("button", { name: models.path.register }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, "n1", "/models/qwen"]);
});

async function registerLocal(storages: unknown[]) {
  api.storages = (async () => list(storages)) as never;
  api.imageVersions = (async () => list([])) as never;
  api.previewLocalModel = (async () => ({ node_uuid: "n1", path: "/models/qwen", source_id: "local/qwen", name: "qwen", weight_bytes: 1024, input_modalities: ["text"] })) as never;
  const bodies: unknown[] = [];
  api.registerLocalModel = (async (body: unknown) => { bodies.push(body); return {}; }) as never;
  render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: models.column.source }));
  fireEvent.click(await screen.findByText(models.register.localPath));
  return bodies;
}
async function submitLocal() {
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: models.detail.node }));
  fireEvent.click(await screen.findByText("gpu3"));
  fireEvent.change(screen.getByPlaceholderText("/models/Qwen/Qwen3-8B"), { target: { value: "/models/qwen" } });
  fireEvent.click(screen.getByRole("button", { name: models.register.parsePreview }));
  fireEvent.click(await screen.findByRole("button", { name: models.register.confirm }));
}

test("registering a node path sends no storage_id with one local storage", async () => {
  const bodies = await registerLocal(oneLocal);
  await new Promise((resolve) => setTimeout(resolve, 30));
  assert.equal(screen.queryByRole("combobox", { name: models.register.storage }), null);
  await submitLocal();
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { node_uuid: "n1", path: "/models/qwen", source_id: "local/qwen", name: "qwen" });
});

test("registering a node path with several local storages sends the chosen storage_id", async () => {
  const bodies = await registerLocal(twoLocal);
  const combo = await screen.findByRole("combobox", { name: models.register.storage });
  fireEvent.mouseDown(combo);
  fireEvent.click(await screen.findByText("nvme"));
  await submitLocal();
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { node_uuid: "n1", path: "/models/qwen", source_id: "local/qwen", name: "qwen", storage_id: 2 });
});
