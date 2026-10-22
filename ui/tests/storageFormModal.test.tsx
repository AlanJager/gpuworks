import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { localStorage, node, nfsStorage } from "./helpers/storageFixtures.ts";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["storage"]);
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: StorageFormModal } = await import("../src/pages/storage/StorageFormModal.tsx");

const s = locale.storage;
const originals = { probe: api.probeStorage, create: api.createStorage, update: api.updateStorage, settings: api.storageSettings, storage: api.storage };
afterEach(() => { cleanup(); api.probeStorage = originals.probe; api.createStorage = originals.create; api.updateStorage = originals.update; api.storageSettings = originals.settings; api.storage = originals.storage; });

// Named nodes whose hostnames differ: the form must show the name a person gave.
const nodes = [node("n1", "gpu-01", { hostname: "10-0-0-1" }), node("n2", "gpu-02", { hostname: "10-0-0-2" })];
const goodProbe = { node_uuid: "n1", node_name: "gpu-01", ok: true, readable: true, writable: true, version: "4.2", total_bytes: 1024 ** 4, available_bytes: 1024 ** 3 * 512 };

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

for (const outcome of ["success", "error"] as const) {
  test(`a late probe ${outcome} cannot populate a reopened local storage form`, async () => {
    const pending = deferred<typeof goodProbe>();
    api.probeStorage = () => pending.promise;
    const { user, view } = renderModal({ storage: nfsStorage() });
    await user.click(button(s.form.probe));
    assert.ok(screen.getByText(s.probe.running));
    const rerender = (open: boolean) => view.rerender(
      <I18nextProvider i18n={i18n}>
        <StorageFormModal open={open} nodes={nodes as never} onClose={() => {}} onSaved={() => {}} />
      </I18nextProvider>,
    );
    rerender(false);
    rerender(true);
    await user.type(field(s.form.name), "new-local");
    await act(async () => {
      if (outcome === "success") pending.resolve(goodProbe);
      else pending.reject(new Error("old probe failed"));
      await pending.promise.catch(() => undefined);
    });
    assert.equal(field(s.form.name).value, "new-local");
    assert.ok(screen.queryByText(s.probe.ok.replace("{{node}}", "gpu-01")) === null, "no old probe result");
    assert.ok(screen.queryByText(s.probe.requestFailed) === null, "no old probe failure");
    assert.ok(screen.queryByText(s.probe.stale) === null, "no old stale probe");
    assert.equal(button(s.form.save).disabled, false);
  });
}

for (const outcome of ["success", "error", "probe-error"] as const) {
  test(`a late save ${outcome} cannot close, contaminate or unlock another storage's pending edit`, async () => {
    const oldSave = deferred<ReturnType<typeof localStorage>>();
    const newSave = deferred<ReturnType<typeof localStorage>>();
    const updatedIds: number[] = [];
    api.updateStorage = (id) => {
      updatedIds.push(id);
      return id === 1 ? oldSave.promise : newSave.promise;
    };
    const first = localStorage();
    const second = localStorage({ id: 3, name: "second" });
    const { user, calls, view } = renderModal({ storage: first });
    await user.click(button(s.form.save));
    assert.deepEqual(updatedIds, [1]);
    const rerender = (open: boolean) => view.rerender(
      <I18nextProvider i18n={i18n}>
        <StorageFormModal open={open} storage={second} nodes={nodes as never}
          onClose={() => { calls.closed += 1; }} onSaved={(row) => calls.saved.push(row)} />
      </I18nextProvider>,
    );
    rerender(false);
    rerender(true);
    assert.equal(field(s.form.name).value, "second");
    assert.equal(button(s.form.save).disabled, false, "the previous edit must not lock a new form");
    await user.click(button(s.form.save));
    assert.deepEqual(updatedIds, [1, 3]);
    await act(async () => {
      if (outcome === "success") oldSave.resolve(first);
      else if (outcome === "probe-error") oldSave.reject(new APIError("STORAGE_PROBE_FAILED", { probe: goodProbe }, "old trial failed", 409));
      else oldSave.reject(new Error("old save failed"));
      await oldSave.promise.catch(() => undefined);
    });
    assert.deepEqual(calls.saved, [], "the previous save must not invoke the parent's close callback");
    assert.equal(calls.closed, 0);
    assert.equal(field(s.form.name).value, "second");
    assert.ok(screen.queryByText(s.form.saveFailed) === null, "no old save failure");
    assert.ok(screen.queryByText(s.probe.ok.replace("{{node}}", "gpu-01")) === null, "no old probe result");
    assert.equal(button(s.form.save).disabled, true, "the old finally must not unlock a new pending save");
    await act(async () => { newSave.resolve(second); await newSave.promise; });
    assert.deepEqual(calls.saved, [second]);
    assert.equal(button(s.form.save).disabled, false);
  });
}

function renderModal(props: Partial<React.ComponentProps<typeof StorageFormModal>> = {}) {
  const calls = { saved: [] as unknown[], closed: 0 };
  const view = render(
    <I18nextProvider i18n={i18n}>
      <StorageFormModal open nodes={nodes as never} onClose={() => { calls.closed += 1; }} onSaved={(row) => calls.saved.push(row)} {...props} />
    </I18nextProvider>,
  );
  return { calls, view, user: userEvent.setup({ document: dom.window.document }) };
}
const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;
// A button that was loading keeps a "loading" icon in its accessible name for a moment, so match the label at the end.
const button = (name: string) => screen.getByRole("button", { name: new RegExp(`${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`) }) as HTMLButtonElement;

// antd's button-style radio hides its input behind a pointer-events:none layer, so activate it directly.
async function chooseType(_user: ReturnType<typeof userEvent.setup>, name: string) {
  fireEvent.click(screen.getByRole("radio", { name }));
}

test("the form follows the chosen type: local has a path only, NFS adds export and options, NFS with cache adds the cache", async () => {
  const { user } = renderModal();
  assert.ok(field(s.form.path), "local storage asks for a directory");
  assert.equal(screen.queryByLabelText(s.form.export), null);
  assert.equal(screen.queryByRole("button", { name: s.form.probe }), null, "a local directory needs no trial mount");

  await chooseType(user, locale.common.storageType.nfs);
  assert.ok(field(s.form.export));
  assert.ok(field(s.form.mountPoint));
  assert.ok(field(s.form.mountOptions));
  assert.equal(screen.queryByLabelText(s.form.cachePath), null);
  assert.ok(button(s.form.probe));

  await chooseType(user, locale.common.storageType.nfs_cache);
  assert.equal(screen.queryByLabelText(s.form.cachePath), null, "cache settings have their own step");
  assert.ok(button(s.cacheDraft.next));

  await chooseType(user, locale.common.storageType.shared_dir);
  assert.ok(field(s.form.sharedPath));
  assert.equal(screen.queryByLabelText(s.form.export), null);
});

test("a local storage saves without a trial mount and posts only its own fields", async () => {
  const posted: unknown[] = [];
  api.createStorage = (async (body: unknown) => { posted.push(body); return localStorage({ id: 5, name: "nvme" }); }) as never;
  const { user, calls } = renderModal();
  assert.equal(button(s.form.save).disabled, false);
  await user.type(field(s.form.name), "nvme");
  await user.type(field(s.form.path), "/mnt/nvme/models");
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(posted, [{ name: "nvme", type: "local", all_nodes: true, targets: [], path: "/mnt/nvme/models" }]);
});

test("invalid fields are explained and nothing is sent", async () => {
  let posted = 0;
  api.createStorage = (async () => { posted += 1; return localStorage(); }) as never;
  const { user } = renderModal();
  await user.type(field(s.form.name), "Bad Name");
  await user.click(button(s.form.save));
  assert.ok(await screen.findByText(s.form.error.name));
  assert.ok(screen.getByText(s.form.error.pathRequired));
  assert.equal(posted, 0);
});

test("an NFS storage cannot be saved before a trial mount succeeds, and a failed one shows why", async () => {
  api.probeStorage = (async () => ({ node_uuid: "n1", node_name: "gpu-01", ok: false, readable: false, writable: false, message: "mount.nfs: access denied", output: "mount.nfs: access denied by server" })) as never;
  const { user } = renderModal();
  await chooseType(user, locale.common.storageType.nfs);
  assert.ok(screen.getByText(s.probe.required));
  assert.equal(button(s.form.save).disabled, true);
  await user.type(field(s.form.name), "archive");
  await user.type(field(s.form.export), "nfs.example.internal:/export/models");
  await user.click(button(s.form.probe));
  assert.ok(await screen.findByText(s.probe.failed.replace("{{node}}", "gpu-01")));
  assert.ok(screen.getByText("mount.nfs: access denied"));
  assert.ok(screen.getByText("mount.nfs: access denied by server"));
  assert.equal(button(s.form.save).disabled, true, "a failed trial mount never allows saving");
});

test("a successful trial mount shows version and capacity, enables saving, and a later edit makes it stale", async () => {
  const probes: unknown[] = [];
  const posted: unknown[] = [];
  api.probeStorage = (async (body: unknown) => { probes.push(body); return goodProbe; }) as never;
  api.createStorage = (async (body: unknown) => { posted.push(body); return nfsStorage(); }) as never;
  const { user, calls } = renderModal();
  await chooseType(user, locale.common.storageType.nfs);
  await user.type(field(s.form.name), "archive");
  await user.type(field(s.form.export), "nfs.example.internal:/export/models");
  await user.click(button(s.form.probe));
  assert.ok(await screen.findByText(s.probe.ok.replace("{{node}}", "gpu-01")));
  assert.ok(screen.getByText("4.2"));
  // No path typed: the server tries its default mount point for the name.
  assert.deepEqual(probes, [{ type: "nfs", path: "", name: "archive", export: "nfs.example.internal:/export/models" }]);
  assert.equal(button(s.form.save).disabled, false);

  await user.type(field(s.form.mountOptions), "vers=3");
  assert.ok(screen.getByText(s.probe.stale));
  assert.equal(button(s.form.save).disabled, true, "the passed probe belongs to the old inputs");
  await user.click(button(s.form.probe));
  await waitFor(() => assert.equal(button(s.form.save).disabled, false));
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(posted, [{ name: "archive", type: "nfs", all_nodes: true, targets: [], export: "nfs.example.internal:/export/models", mount_options: "vers=3" }]);
});

test("a read-only trial result says it will be saved read-only", async () => {
  api.probeStorage = (async () => ({ ...goodProbe, writable: false })) as never;
  const { user } = renderModal();
  await chooseType(user, locale.common.storageType.nfs);
  await user.type(field(s.form.name), "library");
  await user.type(field(s.form.export), "nfs.example.internal:/export/ro");
  await user.click(button(s.form.probe));
  assert.ok(await screen.findByText(s.probe.readOnlyNotice));
  assert.ok(button(s.form.saveReadOnly), "the save button names the read-only outcome");
  assert.equal(button(s.form.saveReadOnly).disabled, false);
});

test("a probe failure reported by the server on save is shown in the same panel", async () => {
  api.probeStorage = (async () => goodProbe) as never;
  api.createStorage = (async () => {
    throw new APIError("STORAGE_PROBE_FAILED", { probe: { node_uuid: "n2", node_name: "gpu-02", ok: false, readable: false, writable: false, message: "timed out", output: "" } }, "timed out", 409);
  }) as never;
  const { user, calls } = renderModal();
  await chooseType(user, locale.common.storageType.nfs);
  await user.type(field(s.form.name), "archive");
  await user.type(field(s.form.export), "nfs.example.internal:/export/models");
  await user.click(button(s.form.probe));
  await waitFor(() => assert.equal(button(s.form.save).disabled, false));
  await user.click(button(s.form.save));
  assert.ok(await screen.findByText(s.probe.failed.replace("{{node}}", "gpu-02")));
  assert.equal(calls.saved.length, 0);
});

test("a probe that cannot run reports the error and keeps saving locked", async () => {
  api.probeStorage = (async () => { throw new APIError("STORAGE_EXPORT_ALREADY_REGISTERED", undefined, "english fallback from the server", 409); }) as never;
  const { user } = renderModal();
  await chooseType(user, locale.common.storageType.shared_dir);
  await user.type(field(s.form.name), "ceph");
  await user.type(field(s.form.sharedPath), "/mnt/ceph");
  await user.click(button(s.form.probe));
  assert.ok(await screen.findByText(s.probe.requestFailed));
  assert.ok(screen.getByText(locale.common.apiError.STORAGE_EXPORT_ALREADY_REGISTERED), "the probe error is translated too");
  assert.equal(button(s.form.save).disabled, true);
});

test("node scope: by label and one by one with a path override and an existing mount", async () => {
  const probes: unknown[] = [];
  const posted: unknown[] = [];
  api.probeStorage = (async (body: unknown) => { probes.push(body); return goodProbe; }) as never;
  api.createStorage = (async (body: unknown) => { posted.push(body); return nfsStorage(); }) as never;
  const { user, calls } = renderModal();
  await chooseType(user, locale.common.storageType.nfs_cache);
  await user.type(field(s.form.name), "team-cache");
  await user.type(field(s.form.export), "h:/x");
  await user.click(button(s.cacheDraft.next));
  fireEvent.click(screen.getByRole("radio", { name: s.form.scope.nodes }));

  // choose a node with the keyboard
  const picker = screen.getByRole("combobox", { name: s.form.scope.chooseNodes });
  fireEvent.mouseDown(picker);
  await user.click((await screen.findAllByText("gpu-01")).at(-1)!);
  await user.click(button(s.cacheDraft.next));
  await user.click(button(s.cacheDraft.configure));
  const editor = screen.getAllByRole("dialog").at(-1)!;
  await user.type(within(editor).getByLabelText(s.form.mountPoint), "/mnt/own");
  fireEvent.click(within(within(editor).getByRole("radiogroup", { name: s.cacheDraft.pathMode })).getByRole("radio", { name: s.cachePolicy.custom }));
  await user.type(within(editor).getByLabelText(s.form.cachePath), "/ssd/cache");
  await user.click(within(editor).getByLabelText(s.cacheDraft.existing));
  await user.click(within(editor).getByRole("button", { name: s.cacheDraft.apply }));
  await user.click(button(s.cacheDraft.next));
  await user.click(button(s.form.probe));
  await waitFor(() => assert.equal(button(s.form.save).disabled, false));
  assert.deepEqual(probes, [{ type: "nfs_cache", path: "/mnt/own", node_uuid: "n1", export: "h:/x", existing: true }]);
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(posted, [{
    name: "team-cache", type: "nfs_cache", all_nodes: false, export: "h:/x", cache_max_bytes: 0, probe_node: "n1",
    targets: [{ node_uuid: "n1", path: "/mnt/own", cache_path: "/ssd/cache", existing: true }],
  }]);
});

test("a label scope needs a label, then sends it as a target", async () => {
  const posted: unknown[] = [];
  api.createStorage = (async (body: unknown) => { posted.push(body); return localStorage(); }) as never;
  const { user, calls } = renderModal();
  await user.type(field(s.form.name), "gpu-pool");
  await user.type(field(s.form.path), "/data");
  fireEvent.click(screen.getByRole("radio", { name: s.form.scope.labels }));
  await user.click(button(s.form.save));
  assert.ok(await screen.findByText(s.form.error.labelsRequired));
  await user.type(screen.getByLabelText(s.form.scope.labelKey), "pool");
  await user.type(screen.getByLabelText(s.form.scope.labelValue), "gpu");
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(posted, [{ name: "gpu-pool", type: "local", all_nodes: false, targets: [{ label_key: "pool", label_value: "gpu" }], path: "/data" }]);
});

test("editing a storage patches its settings, keeps the export read-only, and needs no trial mount", async () => {
  const patches: unknown[] = [];
  api.updateStorage = (async (id: number, body: unknown) => { patches.push([id, body]); return nfsStorage({ name: "renamed" }); }) as never;
  const { user, calls } = renderModal({ storage: nfsStorage() });
  assert.equal(field(s.form.export).disabled, true);
  assert.equal(screen.queryByRole("radio", { name: locale.common.storageType.nfs }), null, "the type is switched on the card, not here");
  const name = field(s.form.name);
  await user.clear(name);
  await user.type(name, "renamed");
  assert.equal(button(s.form.save).disabled, false);
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(patches, [[2, { name: "renamed" }]]);
});

test("editing a storage every node uses shows the nodes with a directory of their own and saves them unchanged", async () => {
  const patches: unknown[] = [];
  const storage = localStorage({ path: "/opt/gpuworks/models", targets: [{ node_uuid: "n1", node_name: "gpu-01", path: "/opt/gpuworks1/agent/models" }] });
  api.updateStorage = (async (id: number, body: unknown) => { patches.push([id, body]); return storage; }) as never;
  const { user, calls } = renderModal({ storage });
  const own = field(s.form.scope.pathOverride.replace("{{node}}", "gpu-01"));
  assert.equal(own.value, "/opt/gpuworks1/agent/models");
  await user.clear(field(s.form.path));
  await user.type(field(s.form.path), "/data/models");
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(patches, [[1, { path: "/data/models" }]], "unchanged node targets are omitted to preserve concurrent node edits");
});

test("a save error stays in the dialog and the dialog can be cancelled from the keyboard", async () => {
  api.createStorage = (async () => { throw new APIError("STORAGE_EXPORT_ALREADY_REGISTERED", undefined, "english fallback from the server", 409); }) as never;
  const { user, calls } = renderModal();
  await user.type(field(s.form.name), "dup");
  await user.type(field(s.form.path), "/data");
  await user.click(button(s.form.save));
  const alert = await screen.findByText(s.form.saveFailed);
  assert.ok(alert);
  const alertBox = alert.closest(".ant-alert") as HTMLElement;
  assert.ok(within(alertBox).getByText(locale.common.apiError.STORAGE_EXPORT_ALREADY_REGISTERED), "the translated error, not the server's English fallback");
  assert.equal(within(alertBox).queryByText(/english fallback/), null);
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(calls.closed, 1));
});

test("a storage refresh while the edit form is open keeps what was typed", async () => {
  const first = nfsStorage();
  const view = renderModal({ storage: first });
  const user = userEvent.setup({ document: dom.window.document });
  const name = field(s.form.name);
  await user.clear(name);
  await user.type(name, "half-typed");
  // The detail page polls: the same storage comes back as a new object.
  const rerender = (storage: typeof first) => view.view.rerender(
    <I18nextProvider i18n={i18n}>
      <StorageFormModal open storage={storage} nodes={nodes as never} onClose={() => {}} onSaved={() => {}} />
    </I18nextProvider>,
  );
  rerender({ ...first, available_bytes: first.available_bytes - 1 });
  rerender({ ...first, name: "renamed-elsewhere" });
  assert.equal(field(s.form.name).value, "half-typed", "a refresh does not reset the form");
  // Another storage does start the form over.
  rerender({ ...first, id: first.id + 1, name: "other" });
  await waitFor(() => assert.equal(field(s.form.name).value, "other"));
});

test("a storage with label and node targets keeps both when edited and saved", async () => {
  const patches: unknown[] = [];
  const storage = localStorage({
    all_nodes: false,
    targets: [{ label_key: "pool", label_value: "gpu" }, { node_uuid: "n1", node_name: "gpu-01", path: "/nvme" }],
  });
  api.updateStorage = (async (id: number, body: unknown) => { patches.push([id, body]); return storage; }) as never;
  const { user, calls } = renderModal({ storage });
  assert.equal((screen.getByLabelText(s.form.scope.labelKey) as HTMLInputElement).value, "pool", "the label target is shown");
  assert.equal(field(s.form.scope.pathOverride.replace("{{node}}", "gpu-01")).value, "/nvme", "and so is the node target");
  await user.clear(field(s.form.name));
  await user.type(field(s.form.name), "renamed");
  await user.click(button(s.form.save));
  await waitFor(() => assert.equal(calls.saved.length, 1));
  assert.deepEqual(patches, [[1, { name: "renamed" }]], "unchanged mixed targets are omitted");
});

test("the form shows the directories the server fills in, not ones it knows itself", async () => {
  api.storageSettings = (async () => ({ control_plane_id: "cp", control_plane_name: "gpuworks-cp", reserve_free_bytes: 0,
    defaults: { local_path: "/srv/models", mount_root: "/srv/mounts", cache_root: "/srv/cache" } })) as never;
  const { user } = renderModal();
  await chooseType(user, locale.common.storageType.nfs_cache);
  await user.type(field(s.form.name), "team");
  await waitFor(() => assert.equal(field(s.form.mountPoint).placeholder, "/srv/mounts/team"));
  await user.type(field(s.form.export), "h:/x");
  await user.click(button(s.cacheDraft.next));
  await user.click(button(s.cacheDraft.next));
  assert.equal((screen.getByRole("textbox", { name: s.form.cachePath }) as HTMLInputElement).placeholder, s.form.cachePathPlaceholder.replace("{{cache}}", "/srv/cache/team"));
});

test("cache default edits keep the opening baseline across polls and send only the changed default", async () => {
  const patches: unknown[] = [];
  const storage = nfsStorage({ type: "nfs_cache", updated_at: "2026-10-16T00:00:00Z", cache_path: "/cache", cache_max_bytes: 200 * 1024 ** 3,
    targets: [{ node_uuid: "n1", override_only: true, cache_max_bytes: 0 }],
    nodes: { total: 2, ready: 2, problems: [], all: [
      { node_uuid: "n1", node_name: "gpu-01", state: "ready", writable: true },
      { node_uuid: "n2", node_name: "gpu-02", state: "ready", writable: true, cache_path: "/cache", cache: { used_bytes: 300 * 1024 ** 3, reclaimable_bytes: 60 * 1024 ** 3, protected_bytes: 240 * 1024 ** 3, limit_bytes: 200 * 1024 ** 3, state: "blocked" } },
    ] } });
  api.updateStorage = (async (id: number, body: unknown) => { patches.push([id, body]); return storage; }) as never;
  const { user, view } = renderModal({ storage });
  const input = screen.getByRole("spinbutton", { name: s.form.cacheMax });
  await user.clear(input);
  await user.type(input, "100");
  view.rerender(<I18nextProvider i18n={i18n}><StorageFormModal open storage={{ ...storage, cache_max_bytes: 900 * 1024 ** 3 }} nodes={nodes as never} onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  assert.equal((screen.getByRole("spinbutton", { name: s.form.cacheMax }) as HTMLInputElement).value, "100");
  assert.ok(screen.getByText(s.cacheDraft.defaultImpact.replace("{{count}}", "1").replace("{{own}}", "1")));
  assert.ok(screen.getByText(s.cacheDraft.trimImpact.replace("{{over}}", "1").replace("{{reclaimable}}", "60.0 GiB").replace("{{protected}}", "240.0 GiB")));
  await user.click(button(s.form.save));
  assert.deepEqual(patches, [[2, { cache_max_bytes: 100 * 1024 ** 3 }]]);
});

test("node target edits send the opening version and surface a concurrency conflict", async () => {
  const patches: unknown[] = [];
  const storage = localStorage({ updated_at: "2026-10-16T00:00:00Z", targets: [{ node_uuid: "n1", path: "/old" }] });
  api.updateStorage = (async (_id: number, body: unknown) => { patches.push(body); throw new Error("Configuration changed elsewhere; reopen to retry"); }) as never;
  const { user, calls } = renderModal({ storage });
  const input = field(s.form.scope.pathOverride.replace("{{node}}", "gpu-01"));
  await user.clear(input);
  await user.type(input, "/new");
  await user.click(button(s.form.save));
  assert.ok(await screen.findByText("Configuration changed elsewhere; reopen to retry"));
  assert.equal(calls.saved.length, 0);
  assert.deepEqual(patches, [{ targets: [{ node_uuid: "n1", path: "/new" }], expected_updated_at: "2026-10-16T00:00:00Z" }]);
});


test("summary cache preview loads facts without replacing a draft, retries failures, and totals known usage", async () => {
  const GIB = 1024 ** 3;
  const summary = nfsStorage({ type: "nfs_cache", cache_path: "/cache", cache_max_bytes: 200 * GIB,
    nodes: { total: 1, ready: 1, problems: [] } });
  const pending = deferred<typeof summary>();
  const reads: unknown[] = [];
  api.storage = (id, all) => { reads.push([id, all]); return reads.length === 1 ? Promise.reject(new Error("capacity unavailable")) : pending.promise; };
  const { user } = renderModal({ storage: summary });
  const input = screen.getByRole("spinbutton", { name: s.form.cacheMax });
  await user.clear(input); await user.type(input, "100");
  assert.ok(await screen.findByText("capacity unavailable"));
  await user.click(button(s.detail.retry));
  assert.ok(screen.getByText(s.cacheDraft.previewLoading));
  assert.equal(screen.queryByText(s.cacheDraft.defaultImpact.replace("{{count}}", "0").replace("{{own}}", "0")), null);
  await act(async () => { pending.resolve({ ...summary, cache_max_bytes: 900 * GIB, nodes: { total: 1, ready: 1, problems: [], all: [
    { node_uuid: "n1", node_name: "gpu-01", state: "ready", writable: true, cache_path: "/cache", cache: { used_bytes: 80 * GIB, reclaimable_bytes: 60 * GIB, protected_bytes: 20 * GIB, limit_bytes: 900 * GIB, state: "satisfied" } },
  ] } }); await pending.promise; });
  assert.deepEqual(reads, [[summary.id, true], [summary.id, true]]);
  assert.equal((input as HTMLInputElement).value, "100");
  assert.ok(screen.getByText(s.cacheDraft.currentUsage.replace("{{used}}", "80.0 GiB").replace("{{reclaimable}}", "60.0 GiB").replace("{{protected}}", "20.0 GiB")));
});

test("default preview treats an inherited but not yet reported directory as unknown", async () => {
  const GIB = 1024 ** 3;
  const storage = nfsStorage({ type: "nfs_cache", cache_path: "/new", cache_max_bytes: 200 * GIB,
    nodes: { total: 1, ready: 1, problems: [], all: [{ node_uuid: "n1", node_name: "gpu-01", state: "ready", writable: true, cache_path: "/old", cache: { used_bytes: 300 * GIB, reclaimable_bytes: 60 * GIB, protected_bytes: 240 * GIB, limit_bytes: 200 * GIB, state: "pending" } }] } });
  const { user } = renderModal({ storage });
  const input = screen.getByRole("spinbutton", { name: s.form.cacheMax });
  await user.clear(input); await user.type(input, "100");
  assert.ok(screen.getByText(s.cacheDraft.unknownImpact.replace("{{unknown}}", "1")));
  assert.equal(screen.queryByText(s.cacheDraft.trimImpact.replace("{{over}}", "1").replace("{{reclaimable}}", "60.0 GiB").replace("{{protected}}", "240.0 GiB")), null);
});


for (const outcome of ["success", "error"] as const) {
  test(`a late cache preview ${outcome} cannot populate another form session`, async () => {
    const summary = nfsStorage({ type: "nfs_cache", cache_path: "/cache", cache_max_bytes: 200 * 1024 ** 3 });
    const pending = deferred<typeof summary>();
    api.storage = () => pending.promise;
    const { view } = renderModal({ storage: summary });
    assert.ok(screen.getByText(s.cacheDraft.previewLoading));
    view.rerender(<I18nextProvider i18n={i18n}><StorageFormModal open storage={localStorage()} nodes={nodes as never} onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
    await act(async () => {
      if (outcome === "success") pending.resolve({ ...summary, nodes: { total: 0, ready: 0, problems: [] } });
      else pending.reject(new Error("old cache preview failed"));
      await pending.promise.catch(() => undefined);
    });
    assert.equal(field(s.form.name).value, "default");
    assert.equal(screen.queryByText(s.cacheDraft.previewFailed), null);
    assert.equal(screen.queryByText("old cache preview failed"), null);
  });
}
