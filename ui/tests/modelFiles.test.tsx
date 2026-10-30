import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["models"]);
const models = locale.models;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { ModelFilesSection } = await import("../src/pages/models/ModelFilesSection.tsx");
const { ModelDetailModal } = await import("../src/pages/ModelsPageParts.tsx");

const api = apiModule.api;
const original = { me: api.me, deleteModelOnNode: api.deleteModelOnNode, syncModel: api.syncModel, downloadModel: api.downloadModel, downloadModelToStorage: api.downloadModelToStorage, deleteModelInStorage: api.deleteModelInStorage };
afterEach(() => { cleanup(); Object.assign(api, original); });

const fill = (template: string, values: Record<string, string | number>) => template.replace(/\{\{(\w+)\}\}/g, (_, key) => String(values[key]));
const GiB = 1024 ** 3;

function row(over: Record<string, unknown>) {
  return { node_uuid: "n1", node_name: "gpu3", state: "present", path: "/data/models/qwen", progress: 1, size_bytes: 15 * GiB, downloaded: 15 * GiB, transferred_bytes: 0, rate_bps: 0, acquisition: "download", deletable: true, ...over };
}
function shared(over: Record<string, unknown>) {
  return { storage_id: 3, storage_name: "archive", storage_type: "nfs", path: "Qwen/Qwen3-8B", state: "present", acquisition: "registered", size_bytes: 15 * GiB, downloaded: 15 * GiB, progress: 1, rate_bps: 0, ready_nodes: 128, cached_nodes: 0, writable: true, ...over };
}
function modelWith(over: Record<string, unknown>) {
  return { id: 9, source: "modelscope", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B", ...over } as never;
}
function stubMe(role: string) { api.me = (async () => ({ role })) as never; }
function renderSection(model: unknown, onChanged = () => {}) {
  return render(<I18nextProvider i18n={i18n}><ModelFilesSection model={model as never} onChanged={onChanged} /></I18nextProvider>);
}
const tbl = (el: HTMLElement) => el.closest("table") as HTMLElement;

test("with a single local storage and nothing shared it is the plain node table without storage headings", async () => {
  stubMe("admin");
  renderSection(modelWith({ nodes: [
    // Times render in the local time zone, so the fixture is a local time.
    row({ storage_id: 1, storage_name: "default", storage_type: "local", last_used_at: new Date(2026, 8, 30, 8, 30).toISOString() }),
    row({ node_uuid: "n2", node_name: "node-01", storage_id: 1, storage_name: "default", storage_type: "local", acquisition: "peer" }),
  ] }));
  assert.ok(screen.getByText(models.files.title));
  assert.equal(screen.getAllByRole("table").length, 1);
  assert.equal(screen.queryByText("default"), null, "the storage name is not repeated when there is only one");
  assert.equal(screen.queryByText("Local storage"), null);
  assert.equal(screen.queryByText(models.files.sharedTitle), null);
  const header = screen.getAllByRole("columnheader").map((c) => c.textContent);
  for (const title of [models.detail.node, models.files.path, models.detail.size, models.detail.status, models.detail.progress, models.column.source, models.files.lastUsed]) assert.ok(header.includes(title), title);
  assert.ok(screen.getByText("gpu3"));
  assert.ok(screen.getByText("node-01"));
  assert.ok(screen.getByText("2026-09-30 08:30"));
  assert.equal(screen.getAllByText(models.files.neverUsed).length, 1, "the file that was never used says so");
  assert.equal(screen.getAllByText("15.0 GiB").length, 2);
});

test("legacy node rows without a storage form one unnamed group even next to a shared storage", () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [row({})], storages: [shared({})] }));
  assert.equal(screen.getAllByRole("table").length, 2);
  assert.ok(screen.getByText(models.files.sharedTitle));
  assert.ok(screen.getByText("gpu3"));
});

test("model files are grouped by storage: local storages by node, a shared storage as one row with availability", async () => {
  stubMe("admin");
  renderSection(modelWith({
    nodes: [
      row({ storage_id: 1, storage_name: "default", storage_type: "local" }),
      row({ node_uuid: "n3", node_name: "node-03", storage_id: 2, storage_name: "nvme", storage_type: "local" }),
      row({ node_uuid: "n1", node_name: "gpu3", storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", cache: true, path: "cache/team-cache/Qwen/Qwen3-8B", last_used_at: "2026-09-29T00:00:00Z" }),
      row({ node_uuid: "n2", node_name: "node-01", storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", cache: true, path: "cache/team-cache/Qwen/Qwen3-8B" }),
    ],
    storages: [shared({}), shared({ storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", ready_nodes: 30, cached_nodes: 2, writer_node: "gpu3" })],
  }));
  // Headings: each local storage, the shared block, and the cache nodes.
  assert.ok(screen.getByText("default")); assert.ok(screen.getByText("nvme"));
  assert.ok(screen.getByText(models.files.sharedTitle));
  assert.equal(screen.getAllByRole("table").length, 4, "default, nvme, shared, team-cache cache nodes");
  // A shared storage is one row, no per-node list.
  const archiveReady = screen.getByText(fill(models.files.readyNodes, { count: 128 }));
  const archiveRow = archiveReady.closest("tr") as HTMLElement;
  assert.ok(within(archiveRow).getByText("archive"));
  assert.ok(within(archiveRow).getByText("NFS"));
  assert.ok(within(archiveRow).getByText("Qwen/Qwen3-8B"));
  assert.ok(within(archiveRow).getByText(models.presence.present));
  assert.equal(within(archiveRow).queryByText(/Cached on/), null, "plain NFS has no cache count");
  // nfs_cache: authoritative row says how many nodes cached it and who wrote it.
  const cacheRow = screen.getByText(fill(models.files.readyNodes, { count: 30 })).closest("tr") as HTMLElement;
  assert.ok(within(cacheRow).getByText(fill(models.files.cachedNodes, { count: 2 })));
  assert.ok(within(cacheRow).getByText(fill(models.files.writerNode, { node: "gpu3" })));
  // ...and the cached nodes are listed below, tagged as cache with their last use.
  assert.equal(screen.getAllByText(models.files.cache).length, 2);
  assert.ok(screen.getByText("node-01"));
  assert.equal(tbl(screen.getByText("node-01")).querySelectorAll("tbody tr").length, 2);
  // Admin gets one delete per shared storage row.
  await waitFor(() => assert.equal(screen.getAllByRole("button", { name: models.action.deleteModelFiles }).length, 2));
});

test("a file the storage no longer holds is shown as missing with the hint to scan again", () => {
  stubMe("admin");
  renderSection(modelWith({ storages: [shared({ state: "missing", ready_nodes: 0 })] }));
  const state = screen.getByText(models.presence.missing);
  assert.ok(state.className.includes("ant-tag-red"), "missing is red");
  assert.ok(screen.getByText(models.files.missingHint));
});

test("only admins get a delete, for shared storage files and for a node's files", async () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [row({ storage_id: 1, storage_name: "default", storage_type: "local" })], storages: [shared({})] }));
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(screen.queryByRole("button", { name: models.action.deleteModelFiles }), null);
  assert.equal(screen.queryByRole("button", { name: models.files.deleteOnNode }), null);
});

test("an admin cannot delete a node's files the server says are not deletable", async () => {
  stubMe("admin");
  renderSection(modelWith({
    nodes: [
      // A read-only (disabled) local storage keeps its files.
      row({ storage_id: 1, storage_name: "default", storage_type: "local", deletable: false }),
      // A node cache stays deletable even when its nfs_cache storage is read-only.
      row({ node_uuid: "n2", node_name: "node-01", storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", cache: true, deletable: true }),
    ],
    storages: [shared({ storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", writable: false })],
  }));
  const buttons = await screen.findAllByRole("button", { name: models.files.deleteOnNode });
  assert.equal(buttons.length, 2);
  const [local, cache] = buttons as HTMLButtonElement[];
  assert.equal(local.disabled, true, "the read-only local storage keeps its files");
  assert.equal(cache.disabled, false, "a node's cache is evictable whatever the storage's setting");
  fireEvent.click(local);
  assert.equal(screen.queryByText(fill(models.files.deleteOnNodeConfirm, { node: "gpu3" })), null, "no confirmation for files that cannot be deleted");
});

test("deleting a node's files asks first and passes the storage; legacy rows pass none", async () => {
  stubMe("admin");
  const calls: unknown[][] = [];
  api.deleteModelOnNode = (async (...args: unknown[]) => { calls.push(args); }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [
    row({ storage_id: 1, storage_name: "default", storage_type: "local" }),
    row({ node_uuid: "n2", node_name: "node-01" }),
  ] }), () => { changed += 1; });
  const [first, second] = await screen.findAllByRole("button", { name: models.files.deleteOnNode });
  fireEvent.click(first);
  assert.ok(await screen.findByText(fill(models.files.deleteOnNodeConfirm, { node: "gpu3" })));
  assert.equal(calls.length, 0, "nothing is deleted before confirming");
  fireEvent.click(screen.getByRole("button", { name: models.action.delete }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, "n1", 1]);
  await waitFor(() => assert.equal(changed, 1));
  fireEvent.click(second);
  fireEvent.click(await screen.findByText(fill(models.files.deleteOnNodeConfirm, { node: "node-01" })).then(() => screen.getAllByRole("button", { name: models.action.delete }).at(-1)!));
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], [9, "n2", undefined]);
});

test("a failed delete reports the server's message and a failed row keeps its retry", async () => {
  stubMe("admin");
  api.deleteModelOnNode = (async () => { throw new Error("MODEL_IN_USE"); }) as never;
  const syncs: unknown[][] = [];
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  renderSection(modelWith({ nodes: [row({ state: "failed", message: "disk_full", acquisition: "peer", source_node_uuid: "n9", last_rate_limit_kib_s: 512, storage_id: 1, storage_name: "default", storage_type: "local" })] }));
  assert.ok(screen.getByText("disk_full"));
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(syncs.length, 1));
  assert.deepEqual(syncs[0], [9, "n1", "n9", 512]);
  fireEvent.click(await screen.findByRole("button", { name: models.files.deleteOnNode }));
  fireEvent.click(await screen.findByRole("button", { name: models.action.delete }));
  assert.ok(await screen.findByText("MODEL_IN_USE"), "the error message is shown");
});

test("retrying a failed public download keeps its non-default storage and exact node", async () => {
  stubMe("admin");
  const downloads: unknown[][] = [];
  const legacy: unknown[][] = [];
  const syncs: unknown[][] = [];
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModel = (async (...args: unknown[]) => { legacy.push(args); }) as never;
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", node_uuid: "n-selected", node_name: "selected-node", storage_id: 7, storage_name: "nvme", storage_type: "local", message: "download failed" })] }), () => { changed += 1; });
  assert.ok(screen.getByText("download failed"));
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(downloads.length + legacy.length + syncs.length, 1));
  assert.deepEqual(downloads, [[9, { storage_id: 7, node_uuids: ["n-selected"] }]]);
  assert.deepEqual(legacy, [], "an explicit storage must not use the default-storage endpoint");
  assert.deepEqual(syncs, [], "public Retry must not copy from a peer");
  await waitFor(() => assert.equal(changed, 1));
});

test("retrying a legacy public download without a storage uses the node download endpoint", async () => {
  stubMe("admin");
  const downloads: unknown[][] = [];
  const storages: unknown[][] = [];
  const syncs: unknown[][] = [];
  api.downloadModel = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModelToStorage = (async (...args: unknown[]) => { storages.push(args); }) as never;
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", node_uuid: "n-legacy" })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(downloads.length + storages.length + syncs.length, 1));
  assert.deepEqual(downloads, [[9, "n-legacy"]]);
  assert.deepEqual(storages, []);
  assert.deepEqual(syncs, []);
  await waitFor(() => assert.equal(changed, 1));
});

test("a failed public retry shows the server error without reporting changed files", async () => {
  stubMe("admin");
  const downloads: unknown[][] = [];
  const syncs: unknown[][] = [];
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); throw new Error("PUBLIC_DOWNLOAD_REJECTED"); }) as never;
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", storage_id: 7 })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  assert.ok(await screen.findByText("PUBLIC_DOWNLOAD_REJECTED"));
  assert.deepEqual(downloads, [[9, { storage_id: 7, node_uuids: ["n1"] }]]);
  assert.deepEqual(syncs, []);
  assert.equal(changed, 0);
  assert.ok(screen.getByRole("button", { name: models.action.retry }), "retry remains available after the rejection");
});

test("cached download rows preserve the existing peer source and rate retry", async () => {
  stubMe("admin");
  const syncs: unknown[][] = [];
  const downloads: unknown[][] = [];
  const legacy: unknown[][] = [];
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModel = (async (...args: unknown[]) => { legacy.push(args); }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", cache: true, storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", source_node_uuid: "n-origin", last_rate_limit_kib_s: 256 })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(syncs.length, 1));
  assert.deepEqual(syncs, [[9, "n1", "n-origin", 256]]);
  assert.deepEqual(downloads, []);
  assert.deepEqual(legacy, []);
  await waitFor(() => assert.equal(changed, 1));
});

test("a rejected peer retry preserves source and rate and does not report changed files", async () => {
  stubMe("admin");
  const syncs: unknown[][] = [];
  const downloads: unknown[][] = [];
  const legacy: unknown[][] = [];
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); throw new Error("PEER_RETRY_REJECTED"); }) as never;
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModel = (async (...args: unknown[]) => { legacy.push(args); }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", acquisition: "peer", storage_id: 7, source_node_uuid: "n-origin", last_rate_limit_kib_s: 512 })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  assert.ok(await screen.findByText("PEER_RETRY_REJECTED"));
  assert.deepEqual(syncs, [[9, "n1", "n-origin", 512]]);
  assert.deepEqual(downloads, []);
  assert.deepEqual(legacy, []);
  assert.equal(changed, 0);
});

test("a model without any file shows the empty state", () => {
  stubMe("admin");
  renderSection(modelWith({}));
  assert.ok(screen.getByText(models.files.empty));
});

test("the detail dialog carries the model files and the copy action", async () => {
  stubMe("admin");
  const copied: unknown[] = [];
  render(<I18nextProvider i18n={i18n}><ModelDetailModal
    model={modelWith({ nodes: [], storages: [shared({})], weight_gib: 15 })}
    onClose={() => {}} onDownload={() => {}} onCopy={(m: unknown) => copied.push(m)} onRegisterPath={() => {}} onChanged={() => {}}
  /></I18nextProvider>);
  assert.ok(screen.getByText(fill(models.files.readyNodes, { count: 128 })));
  const copy = screen.getByRole("button", { name: new RegExp(models.action.copyTo) });
  copy.focus();
  await userEvent.keyboard("{Enter}");
  assert.equal(copied.length, 1);
});

test("a transfer in flight shows its rate next to the bytes; a finished file shows none", () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [
    row({ state: "downloading", progress: 0.5, downloaded: 7.5 * GiB, rate_bps: 48 * 1024 ** 2 }),
    row({ node_uuid: "n2", node_name: "node-01", state: "syncing", progress: 0.25, downloaded: 3.75 * GiB, rate_bps: 1.5 * GiB }),
    row({ node_uuid: "n3", node_name: "node-02", rate_bps: 9 * 1024 ** 2 }),
  ], storages: [shared({ state: "downloading", progress: 0.1, downloaded: 1.5 * GiB, rate_bps: 512 * 1024 })] }));
  assert.ok(screen.getByText("7.5 GiB / 15.0 GiB · 48.0 MiB/s"));
  assert.ok(screen.getByText("3.8 GiB / 15.0 GiB · 1.5 GiB/s"));
  assert.ok(screen.getByText("1.5 GiB / 15.0 GiB · 512.0 KiB/s"), "a shared storage transfer shows its rate too");
  assert.equal(screen.queryByText(/9\.0 MiB\/s/), null, "a present file reports no rate");
});

test("the detail dialog names the published and added times as the list does", () => {
  stubMe("viewer");
  // Dates render in the local time zone, so the fixtures are local noons.
  const localNoon = (year: number, month: number, day: number) => new Date(year, month - 1, day, 12).toISOString();
  render(<I18nextProvider i18n={i18n}><ModelDetailModal
    model={modelWith({ nodes: [], storages: [], origin_updated_at: localNoon(2026, 5, 2), created_at: localNoon(2026, 10, 1) })}
    onClose={() => {}} onDownload={() => {}} onCopy={() => {}} onRegisterPath={() => {}} onChanged={() => {}}
  /></I18nextProvider>);
  const label = (text: string) => screen.getByText(text).closest("th")!.nextElementSibling!.textContent;
  assert.equal(models.detail.originCreatedAt, models.column.publishedAt);
  assert.equal(models.detail.createdAt, models.column.addedAt);
  assert.equal(label(models.detail.originCreatedAt), "2026-05-02", "without a creation date the published time falls back to the last update, as the list does");
  assert.equal(label(models.detail.createdAt), "2026-10-01");
});

test("a transfer names where its bytes come from: the mirror a download uses, the node a copy comes from", () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [
    row({ state: "downloading", progress: 0.5, downloaded: 7.5 * GiB, message: "downloading from hf-mirror.com" }),
    row({ node_uuid: "n2", node_name: "node-01", state: "syncing", acquisition: "peer", source_node_uuid: "n1", source_node_name: "gpu3" }),
    row({ node_uuid: "n3", node_name: "node-02", message: "" }),
  ] }));
  const rowOf = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;
  assert.ok(within(rowOf("gpu3")).getByText(`${models.acquisition.download} · ${fill(models.files.fromHost, { host: "hf-mirror.com" })}`));
  assert.ok(within(rowOf("node-01")).getByText(`${models.acquisition.peer} · ${fill(models.files.fromNode, { node: "gpu3" })}`));
  assert.ok(within(rowOf("node-02")).getByText(models.acquisition.download), "nothing known beyond how it was acquired");
});

test("download cancellation names local and shared locations and refreshes after confirmation", async () => {
  stubMe("viewer");
  const originalCancel = api.cancelModelDownload;
  const calls: unknown[] = [];
  let changes = 0;
  api.cancelModelDownload = (async (id, body) => { calls.push([id, body]); return { state: "canceled", message: "canceled" }; }) as typeof api.cancelModelDownload;
  try {
    renderSection(modelWith({ nodes: [row({ storage_id: 2, state: "downloading" })], storages: [shared({ state: "pending", acquisition: "download" })] }), () => { changes++; });
    const buttons = screen.getAllByRole("button", { name: models.presenceAction.cancelDownload });
    assert.equal(buttons.length, 2);
    fireEvent.click(buttons[0]);
    const confirm = await screen.findByText(models.presenceAction.cancelDownloadConfirm);
    fireEvent.click(within(confirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: models.presenceAction.cancelDownload }));
    await waitFor(() => assert.equal(calls.length, 1));
    assert.deepEqual(calls[0], [9, { storage_id: 2, node_uuid: "n1" }]);
    assert.equal(changes, 1);
    cleanup();
    renderSection(modelWith({ storages: [shared({ state: "downloading", acquisition: "download" })] }), () => { changes++; });
    fireEvent.click(screen.getByRole("button", { name: models.presenceAction.cancelDownload }));
    const sharedConfirm = await screen.findByText(models.presenceAction.cancelDownloadConfirm);
    fireEvent.click(within(sharedConfirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: models.presenceAction.cancelDownload }));
    await waitFor(() => assert.equal(calls.length, 2));
    assert.deepEqual(calls[1], [9, { storage_id: 3, node_uuid: undefined }]);
    assert.equal(changes, 2);
  } finally { api.cancelModelDownload = originalCancel; }
});

test("download cancellation errors preserve the task and do not announce a change", async () => {
  stubMe("viewer");
  const originalCancel = api.cancelModelDownload;
  let changes = 0;
  api.cancelModelDownload = async () => { throw new Error("writer cancellation unavailable"); };
  try {
    renderSection(modelWith({ nodes: [row({ storage_id: 2, state: "downloading" })] }), () => { changes++; });
    fireEvent.click(screen.getByRole("button", { name: models.presenceAction.cancelDownload }));
    const confirm = await screen.findByText(models.presenceAction.cancelDownloadConfirm);
    fireEvent.click(within(confirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: models.presenceAction.cancelDownload }));
    assert.ok(await screen.findByText("writer cancellation unavailable"));
    assert.equal(changes, 0);
    assert.ok(screen.getAllByText(models.presence.downloading).length > 0);
  } finally { api.cancelModelDownload = originalCancel; }
});
