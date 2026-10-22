import assert from "node:assert/strict";
import { test } from "node:test";
import {
  buildCreateRequest, buildPatchRequest, buildProbeRequest, emptyForm, formFromStorage, probeSignature, validateForm,
} from "../src/pages/storage/storageForm.ts";
import { nfsStorage, localStorage } from "./helpers/storageFixtures.ts";

test("a new local storage needs a name and an absolute path, nothing else", () => {
  const form = { ...emptyForm("local"), name: "nvme", path: "/mnt/nvme/models" };
  assert.deepEqual(validateForm(form, "create"), {});
  assert.deepEqual(buildCreateRequest(form), { name: "nvme", type: "local", all_nodes: true, targets: [], path: "/mnt/nvme/models" });
  assert.equal(validateForm({ ...form, path: "" }, "create").path, "pathRequired");
  assert.equal(validateForm({ ...form, path: "models" }, "create").path, "pathAbsolute");
  assert.equal(validateForm({ ...form, name: "Bad Name" }, "create").name, "name");
});

test("an NFS cache storage sends its export, options and cache limit in bytes", () => {
  const form = { ...emptyForm("nfs_cache"), name: "team-cache", export: "nfs.example.internal:/export/models", mountOptions: "vers=4.2", cachePath: "/data/cache", cacheMaxGiB: 200 };
  assert.deepEqual(validateForm(form, "create"), {});
  assert.deepEqual(buildCreateRequest(form), {
    name: "team-cache", type: "nfs_cache", all_nodes: true, targets: [], export: "nfs.example.internal:/export/models",
    mount_options: "vers=4.2", cache_path: "/data/cache", cache_max_bytes: 200 * 1024 ** 3,
  });
  assert.equal(validateForm({ ...form, export: "no-colon-path" }, "create").export, "export");
  // An unlimited cache is an explicit zero, so a later edit can clear a limit.
  assert.equal(buildCreateRequest({ ...form, cacheMaxGiB: null }).cache_max_bytes, 0);
});

test("a shared directory needs its path and carries no export", () => {
  const form = { ...emptyForm("shared_dir"), name: "ceph", path: "/mnt/ceph/models" };
  assert.deepEqual(buildCreateRequest(form), { name: "ceph", type: "shared_dir", all_nodes: true, targets: [], path: "/mnt/ceph/models" });
  assert.equal(validateForm({ ...form, path: "" }, "create").path, "pathRequired");
});

test("node scope: labels and single nodes become targets, per-node overrides ride along", () => {
  const base = { ...emptyForm("nfs_cache"), name: "a", export: "h:/x" };
  const labelled = { ...base, scope: "labels" as const, labels: [{ key: "pool", value: "gpu" }, { key: " ", value: "ignored" }] };
  assert.deepEqual(buildCreateRequest(labelled).targets, [{ label_key: "pool", label_value: "gpu" }]);
  assert.equal(buildCreateRequest(labelled).all_nodes, false);
  assert.equal(validateForm({ ...labelled, labels: [{ key: "", value: "" }] }, "create").targets, "labelsRequired");

  const nodes = { ...base, scope: "nodes" as const, nodes: [
    { node_uuid: "n1", path: "/mnt/a", cache_path: "/ssd/cache", existing: true },
    { node_uuid: "n2", path: "", cache_path: "", existing: false },
  ] };
  assert.deepEqual(buildCreateRequest(nodes, "n1").targets, [
    { node_uuid: "n1", path: "/mnt/a", cache_path: "/ssd/cache", existing: true },
    { node_uuid: "n2" },
  ]);
  assert.equal(buildCreateRequest(nodes, "n1").probe_node, "n1");
  assert.equal(validateForm({ ...nodes, nodes: [] }, "create").targets, "nodesRequired");
  assert.equal(validateForm({ ...nodes, nodes: [{ node_uuid: "n1", path: "rel", cache_path: "", existing: false }] }, "create").overrides, "pathAbsolute");
});

test("the probe asks about the first chosen node and its own mount, and is only good for the same inputs", () => {
  const form = { ...emptyForm("nfs"), name: "archive", export: "h:/x", scope: "nodes" as const, nodes: [{ node_uuid: "n1", path: "/mnt/own", cache_path: "", existing: true }] };
  assert.deepEqual(buildProbeRequest(form), { type: "nfs", path: "/mnt/own", node_uuid: "n1", export: "h:/x", existing: true });
  // Without a path the server picks the mount point; the form names the storage.
  assert.deepEqual(buildProbeRequest({ ...emptyForm("nfs"), name: "archive", export: "h:/x" }), { type: "nfs", path: "", name: "archive", export: "h:/x" });
  assert.notEqual(probeSignature(form), probeSignature({ ...form, export: "h:/y" }));
  assert.equal(probeSignature(form), probeSignature({ ...form, cacheMaxGiB: 50 }), "a cache limit does not need another trial mount");
});

test("editing keeps the current path and leaves the export and type to the card", () => {
  const form = formFromStorage(nfsStorage({ mount_options: "vers=4.2" }));
  assert.equal(form.export, "nfs.example.internal:/export/models");
  assert.deepEqual(validateForm(form, "edit"), {});
  assert.deepEqual(buildPatchRequest({ ...form, name: "renamed" }), {
    name: "renamed", path: "/mnt/gpuworks/model-archive", all_nodes: true, targets: [], mount_options: "vers=4.2",
  });
  const local = formFromStorage(localStorage({ all_nodes: false, targets: [{ node_uuid: "n1", node_name: "gpu1", path: "/nvme" }] }));
  assert.equal(local.scope, "nodes");
  assert.deepEqual(buildPatchRequest(local).targets, [{ node_uuid: "n1", path: "/nvme" }]);
});

test("editing a storage every node uses keeps the nodes that have a directory of their own", () => {
  // The default local storage after an upgrade: every node, and the nodes
  // whose agents kept models elsewhere carry their own directory.
  const storage = localStorage({
    all_nodes: true, path: "/opt/gpuworks/models",
    targets: [
      { node_uuid: "n1", node_name: "gpu3", path: "/opt/gpuworks1/agent/models" },
      { node_uuid: "n2", node_name: "renter", path: "/home/renter/gpuworks/models" },
    ],
  });
  const form = formFromStorage(storage);
  assert.equal(form.scope, "all");
  assert.deepEqual(validateForm(form, "edit"), {});
  assert.deepEqual(buildPatchRequest({ ...form, name: "default" }).targets, [
    { node_uuid: "n1", path: "/opt/gpuworks1/agent/models" },
    { node_uuid: "n2", path: "/home/renter/gpuworks/models" },
  ], "saving must not move any node's model files");
  // A node row is sent as it is, even one with no override: the form never decides which targets to drop.
  const picked = { ...form, nodes: [...form.nodes, { node_uuid: "n3", path: "", cache_path: "", existing: false }] };
  assert.equal(buildPatchRequest(picked).targets?.length, 3);
  // Its directory must be absolute like any other.
  const bad = { ...form, nodes: [{ node_uuid: "n1", path: "relative", cache_path: "", existing: false }] };
  assert.equal(validateForm(bad, "edit").overrides, "pathAbsolute");
});

test("a storage with label and node targets round-trips: all three kinds go back as they came", () => {
  const targets = [
    { label_key: "pool", label_value: "gpu" },
    { label_key: "rack", label_value: "" },
    { node_uuid: "n1", node_name: "gpu1", path: "/nvme", cache_path: "/ssd/cache", cache_max_bytes: 5 * 1024 ** 3, existing: true },
    { node_uuid: "n2", node_name: "gpu2" },
  ];
  const wire = [
    { label_key: "pool", label_value: "gpu" },
    { label_key: "rack", label_value: "" },
    { node_uuid: "n1", path: "/nvme", cache_path: "/ssd/cache", cache_max_bytes: 5 * 1024 ** 3, existing: true },
    { node_uuid: "n2" },
  ];
  for (const all_nodes of [false, true]) {
    const storage = nfsStorage({ type: "nfs_cache", all_nodes, targets });
    const form = formFromStorage(storage);
    assert.equal(form.scope, "mixed");
    assert.equal(form.allNodes, all_nodes);
    assert.deepEqual(validateForm(form, "edit"), {});
    const patch = buildPatchRequest(form);
    assert.equal(patch.all_nodes, all_nodes);
    assert.deepEqual(patch.targets, wire, "editing and saving changes nothing about who the storage applies to");
  }
});

test("every node plus labels is a mixed storage too, and plain shapes keep their scope", () => {
  const labels = [{ label_key: "pool", label_value: "gpu" }];
  const mixed = formFromStorage(localStorage({ all_nodes: true, targets: labels }));
  assert.equal(mixed.scope, "mixed");
  assert.deepEqual(buildPatchRequest(mixed).targets, labels);
  assert.equal(buildPatchRequest(mixed).all_nodes, true);
  assert.equal(formFromStorage(localStorage({ all_nodes: false, targets: labels })).scope, "labels");
  assert.equal(formFromStorage(localStorage({ all_nodes: true, targets: [] })).scope, "all");
  assert.equal(formFromStorage(localStorage({ all_nodes: false, targets: [{ node_uuid: "n1", node_name: "gpu1" }] })).scope, "nodes");
  // A mixed storage must still apply somewhere.
  const empty = { ...mixed, allNodes: false, labels: [{ key: "", value: "" }], nodes: [] };
  assert.equal(validateForm(empty, "edit").targets, "nodesRequired");
});

test("label scope keeps inactive pure overrides without turning them into membership", () => {
  const original = nfsStorage({ type: "nfs_cache", all_nodes: false, targets: [
    { label_key: "pool", label_value: "gpu" },
    { node_uuid: "outside", override_only: true, cache_max_bytes: 0, cache_path: "/own" },
  ] });
  const form = formFromStorage(original);
  assert.equal(form.scope, "labels");
  assert.deepEqual(buildPatchRequest(form).targets, original.targets);
  assert.equal(buildPatchRequest(form).targets?.[1].cache_max_bytes, 0);
  assert.equal(validateForm({ ...form, scope: "nodes" }, "edit").targets, "nodesRequired");
});

test("cache limits reject invalid, negative and unsafe byte amounts without treating them as unlimited", () => {
  const base = { ...emptyForm("nfs_cache"), name: "cache", export: "h:/x" };
  for (const value of [-1, NaN, Infinity, Number.MAX_SAFE_INTEGER]) {
    assert.equal(validateForm({ ...base, cacheMaxGiB: value }, "create").cacheMax, "cacheLimit");
    assert.equal(validateForm({ ...base, nodes: [{ node_uuid: "n1", path: "", cache_path: "", existing: false, cache_max_bytes: value * 1024 ** 3 }] }, "create").overrides, "cacheLimit");
  }
  assert.deepEqual(validateForm({ ...base, cacheMaxGiB: 0.5 }, "create"), {});
});

test("dirty default patches omit unchanged targets and unrelated defaults", () => {
  const baseline = formFromStorage(nfsStorage({ type: "nfs_cache", cache_max_bytes: 200 * 1024 ** 3, targets: [{ node_uuid: "n1", override_only: true, cache_max_bytes: 0 }] }));
  assert.deepEqual(buildPatchRequest({ ...baseline, cacheMaxGiB: 300 }, baseline), { cache_max_bytes: 300 * 1024 ** 3 });
  assert.deepEqual(buildPatchRequest(baseline, baseline), {});
  const inherit = { ...baseline, nodes: baseline.nodes.map((n) => ({ ...n, cache_max_bytes: undefined })) };
  assert.deepEqual(buildPatchRequest(inherit, baseline), { targets: [{ node_uuid: "n1", override_only: true }] });
});
