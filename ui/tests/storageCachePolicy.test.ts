import assert from "node:assert/strict";
import { test } from "node:test";
import { nfsStorage, nodeStatus } from "./helpers/storageFixtures.ts";
import { cacheDiskKnown, cacheNodeRows, cacheOverrideRequest, cachePolicyImpact, filterCacheRows } from "../src/pages/storage/storageCachePolicy.ts";
import { cacheLimitOf } from "../src/pages/storage/storageFiles.ts";

const storage = nfsStorage({ type: "nfs_cache", cache_path: "/cache", cache_max_bytes: 100,
  targets: [{ node_uuid: "unlimited", override_only: true, cache_max_bytes: 0 }, { node_uuid: "inactive", override_only: true, cache_max_bytes: 20 }],
  nodes: { total: 3, ready: 2, problems: [], all: [
    nodeStatus("inherit", "gpu-a", "ready", { cache_path: "/cache", cache_total_bytes: 1000, cache: {used_bytes: 80, protected_bytes: 30, reclaimable_bytes: 50, limit_bytes: 100, state: "satisfied"} }),
    nodeStatus("unlimited", "gpu-b", "ready", {cache: {used_bytes: 80, protected_bytes: 80, reclaimable_bytes: 0, limit_bytes: 0, state: "satisfied"}}),
    nodeStatus("offline", "gpu-c", "offline"),
  ] },
});

test("zero overrides defaults, inactive exceptions do not increase membership, filters compose", () => {
  const rows = cacheNodeRows(storage, []);
  assert.equal(rows.length, 4);
  assert.equal(rows.filter((r) => r.active).length, 3);
  assert.equal(cacheLimitOf(storage, "unlimited"), 0);
  assert.deepEqual(filterCacheRows(rows, "GPU", ["inherit"], ["offline"]).map((r) => r.node_uuid), ["offline"]);
  assert.equal(filterCacheRows(rows, "", ["custom"], ["inactive"])[0].node_uuid, "inactive");
});

test("lowering defaults previews inherited nodes only and does not count missing facts as zero", () => {
  const rows = cacheNodeRows(storage, []);
  assert.deepEqual(cachePolicyImpact(storage, rows, 25, true, true), {count: 2, over: 1, blocked: 1, unknown: 1, offline: 1});
  assert.equal(cachePolicyImpact(storage, rows, 0, true, true).blocked, 0);
  assert.ok(cacheDiskKnown(storage, rows[0]));
  assert.equal(cacheDiskKnown({...storage, cache_path: "/new"}, rows[0]), false);
  assert.equal(cacheDiskKnown(storage, {...rows[0], state: "offline"}), false);
});

test("atomic batch payload freezes IDs, omits unchanged fields, resets with null and preserves zero", () => {
  const ids = ["inherit", "unlimited"];
  const patch = cacheOverrideRequest(ids, true, 0, "keep", "");
  ids.push("new");
  assert.deepEqual(patch, {node_uuids: ["inherit", "unlimited"], cache_max_bytes: 0});
  assert.deepEqual(cacheOverrideRequest(["inactive"], false, 0, "inherit", ""), {node_uuids: ["inactive"], cache_path: null});
  assert.deepEqual(cacheOverrideRequest(["inherit"], true, undefined, "custom", " /other "), {node_uuids: ["inherit"], cache_max_bytes: null, cache_path: "/other"});
});

test("offline observations and changed directories never produce definite capacity impact", () => {
  const row = cacheNodeRows(storage, [])[0];
  assert.deepEqual(cachePolicyImpact(storage, [{...row,state:"offline"}], 10, true), {count:1,over:0,blocked:0,unknown:1,offline:1});
  assert.deepEqual(cachePolicyImpact(storage, [row], 10, true, false, {mode:"custom",value:"/new"}), {count:1,over:0,blocked:0,unknown:1,offline:0});
  assert.deepEqual(cachePolicyImpact(storage, [row], undefined, false, true, {mode:"custom",value:"/new"}), {count:1,over:0,blocked:0,unknown:1,offline:0});
});
