import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n } from "./helpers/dom.ts";
import { localStorage, nfsStorage } from "./helpers/storageFixtures.ts";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["nodes"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: NodeDiskUsage } = await import("../src/pages/nodes/NodeDiskUsage.tsx");
import type { Node } from "../src/api";
import { list } from "./helpers/listPage.ts";

const d = locale.nodes.diskUsage;
const GIB = 1024 ** 3;
const originals = { storageUsage: api.storageUsage, storages: api.storages };
afterEach(() => { cleanup(); Object.assign(api, originals); });

const node = {
  uuid: "n1", hostname: "gpu-01", disk_total_gib: 100,
  path_usage: [
    { kind: "models", path: "/data/models", total_bytes: 100 * GIB, available_bytes: 60 * GIB },
    { kind: "docker", path: "/var/lib/docker", total_bytes: 200 * GIB, available_bytes: 20 * GIB },
    { kind: "udocker", path: "/root/.udocker", total_bytes: 200 * GIB, available_bytes: 150 * GIB },
    { kind: "host", path: "/Users/me/.gpuworks/host-envs", total_bytes: 900 * GIB, available_bytes: 700 * GIB },
    { kind: "storage", storage_id: 2, path: "/mnt/gpuworks/model-archive", total_bytes: 117 * 1024 * GIB, available_bytes: 48 * 1024 * GIB },
    { kind: "storage_cache", storage_id: 3, path: "/data/cache", total_bytes: 1000 * GIB, available_bytes: 900 * GIB },
  ],
} as unknown as Node;
const usageRow = { node_uuid: "n1", hostname: "gpu-01", capacity_known: true, total_bytes: 100 * GIB, available_bytes: 30 * GIB, used_bytes: 70 * GIB, model_bytes: 40 * GIB, image_bytes: 10 * GIB, other_bytes: 20 * GIB };

function renderUsage(n: Node = node) {
  return render(<I18nextProvider i18n={i18n}><NodeDiskUsage node={n} /></I18nextProvider>);
}

test("shows the models disk split into models, images, other and available, and a bar of what is used", async () => {
  api.storageUsage = (async () => list([{ ...usageRow, node_uuid: "other" }, usageRow])) as never;
  api.storages = (async () => list([])) as never;
  renderUsage({ ...node, path_usage: [] });
  assert.ok(screen.getByLabelText(d.loading), "loading first");
  assert.ok(await screen.findByText("70.0 GiB / 100.0 GiB"));
  assert.ok(screen.getByText("Managed models 40.0 GiB · images 10.0 GiB · other 20.0 GiB · available 30.0 GiB"));
  assert.equal(screen.getByRole("progressbar", { name: d.barAria }).getAttribute("aria-valuenow"), "70");
});

test("lists the measured paths with a readable kind, resolving storage names, and a bar each", async () => {
  api.storageUsage = (async () => list([usageRow])) as never;
  api.storages = (async () => list([localStorage({ id: 1 }), nfsStorage({ id: 2 }), nfsStorage({ id: 3, name: "team-cache", type: "nfs_cache" })])) as never;
  renderUsage();
  const row = async (path: string) => (await screen.findByText(path)).closest("tr") as HTMLElement;
  assert.ok(within(await row("/data/models")).getByText(d.kind.models));
  assert.ok(within(await row("/var/lib/docker")).getByText(d.kind.docker));
  assert.ok(within(await row("/root/.udocker")).getByText(d.kind.udocker));
  assert.ok(within(await row("/Users/me/.gpuworks/host-envs")).getByText(d.kind.host), "a Mac's environment packages");
  const storage = await row("/mnt/gpuworks/model-archive");
  assert.ok(within(storage).getByText(d.kind.storage.replace("{{name}}", "model-archive")));
  assert.ok(within(storage).getByText("69.0 TiB / 117.0 TiB"));
  assert.ok(within(await row("/data/cache")).getByText(d.kind.storageCache.replace("{{name}}", "team-cache")));
  const docker = await row("/var/lib/docker");
  assert.equal(within(docker).getByRole("progressbar").getAttribute("aria-valuenow"), "90");
});

test("a node that reports nothing says so; a failed load is a warning, not a crash", async () => {
  api.storageUsage = (async () => list([])) as never;
  api.storages = (async () => list([])) as never;
  const first = renderUsage({ ...node, path_usage: undefined });
  assert.ok(await screen.findByText("Disk capacity unknown"));
  first.unmount();

  api.storageUsage = (async () => { throw new Error("usage down"); }) as never;
  renderUsage();
  assert.ok(await screen.findByText(d.loadFailed));
  assert.ok(screen.getByText("usage down"));
});

test("an unreported models disk stays unknown even when the root disk has a size, and reloads for another node", async () => {
  const calls: number[] = [];
  api.storageUsage = (async () => { calls.push(1); return list([{ ...usageRow, capacity_known: false, total_bytes: 0, available_bytes: 0 }, { ...usageRow, node_uuid: "n2", total_bytes: 10 * GIB, available_bytes: 5 * GIB }]); }) as never;
  api.storages = (async () => list([])) as never;
  const view = renderUsage({ ...node, path_usage: [] });
  assert.ok(await screen.findByText("Disk capacity unknown"));
  assert.equal(screen.queryByRole("progressbar"), null);
  view.rerender(<I18nextProvider i18n={i18n}><NodeDiskUsage node={{ ...node, uuid: "n2", path_usage: [] }} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("5.0 GiB / 10.0 GiB")));
  assert.equal(calls.length, 2);
});


test("a never-reported node and an unavailable path show unknown capacity, while a measured empty disk shows zero used", async () => {
  api.storageUsage = (async () => list([{ ...usageRow, capacity_known: false, total_bytes: 0, available_bytes: 0, used_bytes: 0, model_bytes: 0, image_bytes: 0, other_bytes: 0 }])) as never;
  api.storages = (async () => list([])) as never;
  const view = renderUsage({ ...node, disk_total_gib: 0, path_usage: [{ kind: "models", path: "/unavailable", total_bytes: 0, available_bytes: 0 }] });
  const path = (await screen.findByText("/unavailable")).closest("tr") as HTMLElement;
  assert.ok(within(path).getByText("Disk capacity unknown"));
  assert.equal(screen.queryByRole("progressbar"), null);
  assert.equal(screen.getAllByText("Disk capacity unknown").length, 2);
  view.unmount();
  api.storageUsage = (async () => list([{ ...usageRow, available_bytes: usageRow.total_bytes, used_bytes: 0, model_bytes: 0, image_bytes: 0, other_bytes: 0 }])) as never;
  renderUsage({ ...node, path_usage: [] });
  assert.ok(await screen.findByText("0 B / 100.0 GiB"));
  assert.equal(screen.queryByText("Disk capacity unknown"), null);
  assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "0");
});
